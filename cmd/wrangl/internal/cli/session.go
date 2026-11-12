package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

// DetachPrefix is the detach chord's first key, Ctrl-]. Claude Code binds neither it nor the tmux
// and zellij prefixes people expect here, and a terminal does not give it a meaning of its own.
// Ctrl-] then d detaches; Ctrl-] twice sends one literal Ctrl-].
const (
	DetachPrefix = 0x1d
	detachKey    = 'd'
)

// chordFilter passes keystrokes through and recognises the detach chord, including when the two
// keys arrive in separate reads.
type chordFilter struct{ pending bool }

// feed returns the bytes to forward to the session and whether to detach.
func (f *chordFilter) feed(p []byte) (out []byte, detach bool) {
	for i, c := range p {
		if f.pending {
			f.pending = false
			switch c {
			case detachKey:
				return out, true
			case DetachPrefix:
				out = append(out, DetachPrefix)
			default: // not a chord: send both keys on
				out = append(out, DetachPrefix, c)
			}
			continue
		}
		if c == DetachPrefix {
			f.pending = true
			continue
		}
		out = append(out, p[i])
	}
	return out, false
}

func sessionCmd(g *Global) *cobra.Command {
	var sock string
	client := func() sessionhost.Client {
		if sock == "" {
			sock = os.Getenv("WRANGL_SESSIONS_SOCKET")
		}
		if sock == "" {
			sock = DefaultSessionSocket()
		}
		return sessionhost.Client{Path: sock}
	}
	unreachable := func(err error) error {
		if err == nil {
			return nil
		}
		var pe *os.PathError
		if errors.As(err, &pe) || strings.Contains(err.Error(), "connect") {
			return fmt.Errorf("%w\n(is the session host running, and are you in the wrangl-sessions group? socket %s)", err, sock)
		}
		return err
	}

	ls := &cobra.Command{
		Use: "ls", Short: "List primary sessions with their state and Remote Control name", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := client().Do(sessionhost.Request{Op: sessionhost.OpList})
			if err != nil {
				return unreachable(err)
			}
			return emit(cmd, g, r.Sessions, func(w io.Writer) {
				fmt.Fprintln(w, "SLUG\tREMOTE CONTROL\tSTATE\tPID\tUP\tCLIENTS\tSIZE")
				for _, s := range r.Sessions {
					state := s.State
					if s.State == "exited" {
						state = fmt.Sprintf("exited (%d)", s.ExitCode)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%dx%d\n", s.Slug, dash(s.Name), state, pid(s.PID), age(s), s.Clients, s.Cols, s.Rows)
				}
			})
		},
	}

	var watch, ring bool
	attach := &cobra.Command{
		Use:   "attach <slug>",
		Short: "Attach this terminal to a primary session (detach: Ctrl-] d)",
		Long: "Attaches to the session's PTY over the session host socket, shared live with every other client.\n" +
			"The terminal is put in raw mode and its size follows this window. Detach with Ctrl-] then d (Ctrl-] twice\n" +
			"types one Ctrl-]); detaching never stops the session. Works over `ssh host -t wrangl session attach <slug>`.\n" +
			"--watch attaches read-only: no typing, no resizing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fd := int(os.Stdin.Fd())
			if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return errors.New("attach needs a terminal; over ssh use `ssh -t`")
			}
			cols, rows, err := term.GetSize(fd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			defer signal.Stop(winch)
			sizes := make(chan [2]int, 1)
			go func() {
				for range winch {
					if c, r, err := term.GetSize(fd); err == nil {
						select {
						case sizes <- [2]int{c, r}:
						default:
						}
					}
				}
			}()
			req := sessionhost.Request{Slug: args[0], Control: !watch, Cols: cols, Rows: rows}
			if ring {
				req.Replay = "ring"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "attaching to %s; detach with Ctrl-] d\r\n", args[0])
			old, err := term.MakeRaw(fd)
			if err != nil {
				return err
			}
			defer term.Restore(fd, old)
			res, err := Attach(ctx, client(), req, os.Stdin, os.Stdout, sizes)
			term.Restore(fd, old)
			fmt.Fprintln(cmd.ErrOrStderr())
			switch {
			case err != nil:
				return unreachable(err)
			case res.Ended:
				fmt.Fprintf(cmd.ErrOrStderr(), "session ended (exit %d)\n", res.ExitCode)
			default:
				fmt.Fprintln(cmd.ErrOrStderr(), "detached; the session is still running")
			}
			return nil
		},
	}
	attach.Flags().BoolVar(&watch, "watch", false, "read-only: do not type into or resize the session")
	attach.Flags().BoolVar(&ring, "ring", false, "replay the raw scrollback instead of a screen snapshot")

	var yes bool
	confirm := func(cmd *cobra.Command, what string) error {
		if yes {
			return nil
		}
		in := cmd.InOrStdin()
		if f, ok := in.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
			return errors.New("not a terminal: pass --yes to confirm")
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", what)
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("cancelled")
		}
		return nil
	}
	restart := &cobra.Command{
		Use: "restart <slug>", Short: "Restart a primary session (the conversation resumes with claude -c)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := confirm(cmd, fmt.Sprintf("Restart %s? The process ends, attached clients are disconnected, and the conversation resumes with `claude -c`.", args[0])); err != nil {
				return err
			}
			r, err := client().Do(sessionhost.Request{Op: sessionhost.OpRestart, Slug: args[0]})
			if err != nil {
				return unreachable(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restarted %s (pid %d), resumed with -c\n", args[0], r.Session.PID)
			return nil
		},
	}
	stop := &cobra.Command{
		Use: "stop <slug>", Short: "Stop a primary session", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := confirm(cmd, fmt.Sprintf("Stop %s? The process ends. If the controller keeps primary sessions for this project it will start it again.", args[0])); err != nil {
				return err
			}
			if _, err := client().Do(sessionhost.Request{Op: sessionhost.OpStop, Slug: args[0]}); err != nil {
				return unreachable(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", args[0])
			return nil
		},
	}
	for _, c := range []*cobra.Command{restart, stop} {
		c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	}

	c := group("session", "Primary Claude sessions on this host (talks to the session host socket)", ls, attach, restart, stop)
	c.PersistentFlags().StringVar(&sock, "socket", "", "session host socket (env WRANGL_SESSIONS_SOCKET; default ~/.local/share/wrangl/sessions.sock)")
	return c
}

func pid(p int) string {
	if p == 0 {
		return "-"
	}
	return fmt.Sprint(p)
}

func age(s sessionhost.Info) string {
	if s.State != "running" || s.Started.IsZero() {
		return "-"
	}
	d := time.Since(s.Started)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// AttachResult says how an attach ended.
type AttachResult struct {
	Ended    bool // the session's process ended (otherwise: detached or the connection closed)
	ExitCode int
}

// Attach connects in/out to a session until the detach chord, EOF on in, the session ending, or ctx.
// sizes delivers window-size changes to forward (controlling attaches only). It does no terminal
// setup, so it can be driven by tests.
func Attach(ctx context.Context, c sessionhost.Client, req sessionhost.Request, in io.Reader, out io.Writer, sizes <-chan [2]int) (AttachResult, error) {
	a, err := c.Attach(req)
	if err != nil {
		return AttachResult{}, err
	}
	defer a.Detach()
	type frameMsg struct {
		typ byte
		p   []byte
		err error
	}
	frames := make(chan frameMsg)
	go func() {
		for {
			t, p, err := a.Next()
			select {
			case frames <- frameMsg{t, p, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	keys := make(chan []byte)
	go func() {
		var f chordFilter
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				p, detach := f.feed(buf[:n])
				if len(p) > 0 {
					select {
					case keys <- p:
					case <-ctx.Done():
						return
					}
				}
				if detach {
					close(keys)
					return
				}
			}
			if err != nil {
				close(keys)
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return AttachResult{}, nil
		case fm := <-frames:
			if fm.err != nil {
				return AttachResult{}, nil
			}
			switch fm.typ {
			case sessionhost.FrameOutput:
				if _, err := out.Write(fm.p); err != nil {
					return AttachResult{}, err
				}
			case sessionhost.FrameEnd:
				var e struct {
					ExitCode int `json:"exit_code"`
				}
				jsonUnmarshal(fm.p, &e)
				return AttachResult{Ended: true, ExitCode: e.ExitCode}, nil
			}
		case p, ok := <-keys:
			if !ok {
				return AttachResult{}, nil
			}
			if req.Control {
				if err := a.Input(p); err != nil {
					return AttachResult{}, nil
				}
			}
		case sz := <-sizes:
			if req.Control {
				a.Resize(sz[0], sz[1])
			}
		}
	}
}

func jsonUnmarshal(b []byte, v any) { _ = json.Unmarshal(b, v) }
