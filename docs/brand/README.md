# wrangl brand

- `source.jpg`: the original artwork (the checkerboard is baked in, not transparency).
- `make-logo.py`: keys out the dark background and writes the mark, lockup and PWA icons into
  `cmd/wrangl/internal/web/static/`.
- **Backronym:** **W**ork, **R**un, **A**sk, **N**udge, **G**ate, **L**and.
  - Work: the hierarchical backlog (release > milestone > feature > task).
  - Run: queue it onto runners.
  - Ask: agents ask you questions; you ask the controller agent about a task.
  - Nudge: steer a run, and approve or reject what a controller agent proposes.
  - Gate: reviews, approvals and auto-merge rules.
  - Land: the PR merges.
