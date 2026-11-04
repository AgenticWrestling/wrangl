package fleet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
)

// Runner names are three words from 80s/90s alt and indie music: a mood, a thing (song and
// album words), and a band. cherry-rain-slowdive, neon-signal-pixies. They carry no meaning, so
// nothing may parse a name for its template: that is a separate field.
var (
	nameMood = []string{"cherry", "velvet", "pale", "sunday", "electric", "neon", "violet", "hollow", "shiny", "paper",
		"golden", "silver", "lazy", "crystal", "midnight", "sugar", "blue", "wild", "faded", "glass", "honey", "ivory",
		"lunar", "magnetic", "northern", "pretty", "quiet", "rusty", "scarlet", "tender", "untitled", "vapor", "wonder",
		"yellow", "amber", "bright", "cosmic", "distant", "endless", "fuzzy", "gentle", "heavy", "indigo", "jangly",
		"kind", "loud", "mellow", "nocturnal"}
	nameThing = []string{"rain", "morning", "bomb", "kiss", "machine", "heaven", "lights", "river", "garden", "radio",
		"summer", "fire", "sky", "dream", "ghost", "highway", "mirror", "thunder", "ocean", "parade", "signal", "static",
		"tangerine", "wave", "window", "zero", "anthem", "balloon", "camera", "daydream", "echo", "flowers", "gravity",
		"harbor", "island", "jukebox", "kingdom", "lullaby", "moon", "orbit", "paradise", "riot", "sparkle", "trampoline",
		"valentine", "wallpaper", "yesterday", "fever"}
	nameBand = []string{"pixies", "cure", "smiths", "slowdive", "ride", "pavement", "nirvana", "breeders", "blur", "suede",
		"pulp", "elastica", "radiohead", "mogwai", "luna", "sebadoh", "stereolab", "lush", "belly", "curve", "swervedriver",
		"chapterhouse", "cranberries", "husker", "replacements", "wilco", "tindersticks", "verve", "supergrass", "mazzy",
		"galaxie", "throwing", "ash", "bjork", "portishead", "garbage", "weezer", "beck", "mercury", "spoon", "sleater",
		"hole", "afghan", "guided", "jesus", "cocteau", "ministry", "dinosaur"}
)

func pick(l []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(l))))
	if err != nil {
		return l[0]
	}
	return l[n.Int64()]
}

// randomName is one candidate name.
func randomName() string { return pick(nameMood) + "-" + pick(nameThing) + "-" + pick(nameBand) }

// newName returns a name no runner (live or terminated) already has.
func (s *Service) newName(ctx context.Context) string {
	for i := 0; i < 40; i++ {
		n := randomName()
		if _, err := s.DB.RQ.GetRunnerByName(ctx, n); errors.Is(err, sql.ErrNoRows) {
			return n
		}
	}
	b := make([]byte, 2) // the lists are nearly exhausted: keep names unique regardless
	io.ReadFull(rand.Reader, b)
	return randomName() + "-" + hex.EncodeToString(b)
}
