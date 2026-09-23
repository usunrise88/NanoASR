package era

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

// The golden files in testdata/golden/era-whisperx are what whisperX's own
// writers produce for these fixtures, with the options whisper-asr-webservice
// passes them. "Follows the reference" is a claim that decays silently, so it is
// checked against the reference's actual output rather than against a
// description of it.
const goldenDir = "../../../testdata/golden/era-whisperx"

func goldenWord(w string, start, end, score float64) core.Word {
	return core.Word{Word: w, Start: start, End: end, Confidence: score}
}

// shortFixture exercises the cue break: three segments whose words run on, with
// a pause of more than three seconds before the third.
func shortFixture() *core.Result {
	return &core.Result{
		Language: "ru",
		Segments: []core.Segment{
			{Start: 0.5, End: 2.04, Text: "Привет, мир", Words: []core.Word{
				goldenWord("Привет,", 0.5, 1.1, 0.9),
				goldenWord("мир", 1.2, 2.04, 0.72),
			}},
			{Start: 2.2, End: 3.0, Text: "как дела?", Words: []core.Word{
				goldenWord("как", 2.2, 2.5, 0.8),
				goldenWord("дела?", 2.6, 3.0, 0.65),
			}},
			{Start: 7.25, End: 9.0, Text: "Всё хорошо", Words: []core.Word{
				goldenWord("Всё", 7.25, 7.6, 0.55),
				goldenWord("хорошо", 7.7, 9.0, 0.91),
			}},
		},
	}
}

// longFixture crosses the hour, where WebVTT starts writing the hours field
// that SubRip always writes.
func longFixture() *core.Result {
	return &core.Result{
		Language: "ru",
		Segments: []core.Segment{
			{Start: 3599.9996, End: 3601.5, Text: "около часа", Words: []core.Word{
				goldenWord("около", 3599.9996, 3600.4, 0.5),
				goldenWord("часа", 3600.6, 3601.5, 0.5),
			}},
		},
	}
}

// wrappingFixture is nine hundred short words with no pause anywhere, so the
// only thing that can break a cue is the thousand-character line width. It
// reaches the two branches the other fixtures never do: the wrap onto a second
// line, and the cue break that follows once a cue holds max_line_count of them.
func wrappingFixture() *core.Result {
	words := make([]core.Word, 0, 900)
	texts := make([]string, 0, 900)
	for i := range 900 {
		w := fmt.Sprintf("сл%d", i)
		start := math.Round(float64(i)*0.2*1000) / 1000
		words = append(words, goldenWord(w, start, math.Round((start+0.15)*1000)/1000, 0.5))
		texts = append(texts, w)
	}
	return &core.Result{
		Language: "ru",
		Segments: []core.Segment{{
			Start: words[0].Start,
			End:   words[len(words)-1].End,
			Text:  strings.Join(texts, " "),
			Words: words,
		}},
	}
}

func TestRenderingMatchesTheWhisperxGolden(t *testing.T) {
	fixtures := map[string]*core.Result{
		"short":    shortFixture(),
		"long":     longFixture(),
		"wrapping": wrappingFixture(),
	}
	for name, res := range fixtures {
		for _, output := range []string{outputTXT, outputSRT, outputVTT, outputTSV, outputJSON} {
			t.Run(name+"/"+output, func(t *testing.T) {
				want, err := os.ReadFile(filepath.Join(goldenDir, name+"."+output))
				if err != nil {
					t.Fatal(err)
				}
				got, err := renderTranscript(res, output)
				if err != nil {
					t.Fatal(err)
				}
				if output == outputJSON {
					// Python's json.dump writes ", " and ": " separators that
					// Go's encoder does not. No parser can observe that, so the
					// json golden is compared as data.
					assertSameJSON(t, string(want), got)
					return
				}
				if got != string(want) {
					t.Errorf("rendering drifted from the reference:\ngot  %q\nwant %q", got, want)
				}
			})
		}
	}
}

func assertSameJSON(t *testing.T, want, got string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(want), &a); err != nil {
		t.Fatalf("golden is not json: %v", err)
	}
	if err := json.Unmarshal([]byte(got), &b); err != nil {
		t.Fatalf("rendering is not json: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("json drifted from the reference:\ngot  %s\nwant %s", got, want)
	}
}
