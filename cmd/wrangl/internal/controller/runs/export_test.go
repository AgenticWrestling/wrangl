package runs

import "context"

// NoteToolError exposes noteToolError to the black-box tests.
func (s *Service) NoteToolError(ctx context.Context, runID int64, tool string, err error) {
	s.noteToolError(ctx, runID, tool, err)
}
