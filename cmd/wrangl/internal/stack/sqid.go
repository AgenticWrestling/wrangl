package stack

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/sqids/sqids-go"
)

// DefaultSqidsAlphabet is sqids' default alphabet; ShuffledAlphabet permutes it once at
// `controller init` and the result is stored in meta.sqids_alphabet.
const DefaultSqidsAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

const sqidMinLength = 6

// ErrBadSqid reports a string that is not a canonical sqid for this alphabet.
var ErrBadSqid = errors.New("invalid sqid")

// Sqids encodes task ids. Task rows store the encoded value, so changing the alphabet
// later never changes an existing task's sqid.
type Sqids struct{ s *sqids.Sqids }

// NewSqids builds an encoder for alphabet.
func NewSqids(alphabet string) (*Sqids, error) {
	s, err := sqidsNew(alphabet, sqidMinLength)
	if err != nil {
		return nil, err
	}
	return &Sqids{s: s}, nil
}

// Encode returns the sqid for a positive row id.
func (s *Sqids) Encode(id int64) (string, error) {
	if id <= 0 {
		return "", fmt.Errorf("%w: id %d", ErrBadSqid, id)
	}
	return s.s.Encode([]uint64{uint64(id)})
}

// Decode returns the row id for a canonical sqid. Non-canonical strings that happen to
// decode (sqids allows several spellings) are rejected, so each task has one sqid.
func (s *Sqids) Decode(sqid string) (int64, error) {
	ids := s.s.Decode(sqid)
	if len(ids) != 1 || ids[0] == 0 || ids[0] > 1<<62 {
		return 0, fmt.Errorf("%w: %q", ErrBadSqid, sqid)
	}
	canon, err := s.s.Encode(ids)
	if err != nil || canon != sqid {
		return 0, fmt.Errorf("%w: %q", ErrBadSqid, sqid)
	}
	return int64(ids[0]), nil
}

// ShuffledAlphabet returns a uniformly random permutation of DefaultSqidsAlphabet using
// randomness from r (crypto/rand.Reader if nil).
func ShuffledAlphabet(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := []byte(DefaultSqidsAlphabet)
	for i := len(b) - 1; i > 0; i-- {
		j, err := rand.Int(r, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		b[i], b[j.Int64()] = b[j.Int64()], b[i]
	}
	return string(b), nil
}

func sqidsNew(alphabet string, min uint8) (*sqids.Sqids, error) {
	return sqids.New(sqids.Options{Alphabet: alphabet, MinLength: min})
}
