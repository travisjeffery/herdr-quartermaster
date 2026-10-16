// Command frames turns a recording made by scripts/demo/record.sh into SVG
// frames and an ffmpeg concat list. The recording is a directory of
// `tmux capture-pane -e` dumps named by the millisecond they were taken at, and
// marks.txt, whose "<ms> cut-start" and "<ms> cut-end" lines bracket waits to
// leave out (the ticker's next GitHub pass, an agent starting). A "<ms> still"
// line picks the frame for still.svg, the README's screenshot.
//
//	go run ./scripts/demo/frames -in DIR -out DIR -w 120 -h 34
//
// It writes NNNN.svg for each distinct frame and concat.txt, which names
// NNNN.png with how long each shows.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/travisjeffery/herdr-quartermaster/internal/ansisvg"
)

type frame struct {
	ms   int
	text string
}

type span struct{ from, to int }

func main() {
	in := flag.String("in", "", "directory of <ms>.ans captures and marks.txt")
	out := flag.String("out", "", "directory for the SVG frames and concat.txt")
	w := flag.Int("w", 120, "terminal columns")
	h := flag.Int("h", 34, "terminal rows")
	title := flag.String("title", "herdr", "window title")
	hold := flag.Int("hold", 2500, "ms to hold the last frame")
	flag.Parse()
	if *in == "" || *out == "" {
		log.Fatal("usage: frames -in DIR -out DIR [-w 120 -h 34]")
	}
	frames, err := readFrames(*in)
	if err != nil {
		log.Fatal(err)
	}
	cuts, still, err := readMarks(filepath.Join(*in, "marks.txt"))
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	// Keep frames outside the cuts, on a timeline with the cuts taken out, and
	// only when the screen changed.
	type shown struct {
		at   int
		text string
	}
	var kept []shown
	for _, f := range frames {
		at, ok := squeeze(f.ms, cuts)
		if !ok {
			continue
		}
		if len(kept) > 0 && kept[len(kept)-1].text == f.text {
			continue
		}
		kept = append(kept, shown{at, f.text})
	}
	if len(kept) == 0 {
		log.Fatal("no frames")
	}
	if still >= 0 {
		var pick string
		for _, f := range frames {
			if f.ms <= still {
				pick = f.text
			}
		}
		svg := ansisvg.SVG(ansisvg.Replay(pick, *w, *h), *title, ansisvg.TokyoNight)
		if err := os.WriteFile(filepath.Join(*out, "still.svg"), []byte(svg), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	var list strings.Builder
	for i, k := range kept {
		name := fmt.Sprintf("%04d", i)
		svg := ansisvg.SVG(ansisvg.Replay(k.text, *w, *h), *title, ansisvg.TokyoNight)
		if err := os.WriteFile(filepath.Join(*out, name+".svg"), []byte(svg), 0o644); err != nil {
			log.Fatal(err)
		}
		dur := *hold
		if i+1 < len(kept) {
			dur = kept[i+1].at - k.at
		}
		fmt.Fprintf(&list, "file '%s.png'\nduration %.3f\n", name, float64(dur)/1000)
	}
	// The concat demuxer drops the last entry's duration unless it is repeated.
	fmt.Fprintf(&list, "file '%04d.png'\n", len(kept)-1)
	if err := os.WriteFile(filepath.Join(*out, "concat.txt"), []byte(list.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d frames, %.1fs\n", len(kept), float64(kept[len(kept)-1].at+*hold)/1000)
}

func readFrames(dir string) ([]frame, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.ans"))
	if err != nil {
		return nil, err
	}
	var out []frame
	for _, p := range paths {
		ms, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(p), ".ans"))
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, frame{ms, string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ms < out[j].ms })
	return out, nil
}

// readMarks reads the cuts and the still's time, -1 when there is none.
func readMarks(path string) ([]span, int, error) {
	still := -1
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, still, nil
	}
	if err != nil {
		return nil, still, err
	}
	defer f.Close()
	var cuts []span
	start := -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ms, mark, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		n, err := strconv.Atoi(ms)
		if err != nil {
			continue
		}
		switch mark {
		case "still":
			still = n
		case "cut-start":
			start = n
		case "cut-end":
			if start >= 0 {
				cuts = append(cuts, span{start, n})
				start = -1
			}
		}
	}
	return cuts, still, sc.Err()
}

// squeeze is ms on the timeline without the cuts; false inside one.
func squeeze(ms int, cuts []span) (int, bool) {
	at := ms
	for _, c := range cuts {
		switch {
		case ms >= c.from && ms < c.to:
			return 0, false
		case ms >= c.to:
			at -= c.to - c.from
		}
	}
	return at, true
}
