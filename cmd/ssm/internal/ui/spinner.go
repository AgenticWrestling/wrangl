// cmd/ssm/internal/ui/spinner.go
package ui

import (
	"fmt"
	"os"
	"time"
)

func Spinner(msg string, f func() error) error {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan error, 1)
	go func() { done <- f() }()

	i := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			fmt.Fprintf(os.Stderr, "\r\033[K")
			return err
		case <-ticker.C:
			fmt.Fprintf(os.Stderr, "\r%s %s", frames[i%len(frames)], msg)
			i++
		}
	}
}
