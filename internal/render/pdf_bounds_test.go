// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	pdfium "github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"

	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/testfixtures"
)

// pdfOf writes objects, numbered from 1, as a PDF with a correct
// cross-reference table. The first object is the catalog.
func pdfOf(objects ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, at := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", at)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func streamOf(dict, body string) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(body), body)
}

// nestedForms is a one-page PDF of under two kilobytes that asks for ten to
// the power of depth drawings: the page draws a form ten times, that form
// draws the next ten times, and the last fills one square.
func nestedForms(depth int) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /XObject << /F 5 0 R >> >> >>",
		streamOf("", strings.Repeat("/F Do\n", 10)),
	}
	for d := 1; d <= depth; d++ {
		if d == depth {
			objects = append(objects, streamOf("/Type /XObject /Subtype /Form /BBox [0 0 612 792]", "0 0 0 rg 100 100 50 50 re f"))
			break
		}
		objects = append(objects, streamOf(
			fmt.Sprintf("/Type /XObject /Subtype /Form /BBox [0 0 612 792] /Resources << /XObject << /F %d 0 R >> >>", 5+d),
			strings.Repeat("/F Do\n", 10)))
	}
	return pdfOf(objects...)
}

// A file of under two kilobytes can ask the engine for more memory than a
// worker has. It is refused, it costs the process almost nothing, and the
// engine reads the next file as if nothing had happened.
func TestAPageWrittenToExhaustTheEngineFailsWithinItsBounds(t *testing.T) {
	normal := testfixtures.Read(t, testfixtures.TextPDF)
	if _, err := engine.Render(context.Background(), normal, detect.MIMEPDF, 1, describe(72, 0, "png")); err != nil {
		t.Fatal(err)
	}
	bomb := nestedForms(6)
	if len(bomb) > 2048 {
		t.Fatalf("the file is %d bytes", len(bomb))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := engine.Render(context.Background(), bomb, detect.MIMEPDF, 1, describe(72, 0, "png"))
	runtime.ReadMemStats(&after)
	if fault.CodeOf(err) != fault.DocumentCorrupt || !strings.Contains(fault.DetailOf(err), "more memory than a render is given") {
		t.Fatalf("the page was not refused for what it asks: %v", err)
	}
	// The instance's memory is bounded and is not the process's heap: what
	// the heap was asked for while the page was refused is a small fraction
	// of the instance's bound.
	const bound = 128 << 20
	if grew := after.TotalAlloc - before.TotalAlloc; grew > bound {
		t.Fatalf("refusing the page allocated %d MiB on the heap, over %d MiB", grew>>20, bound>>20)
	}

	if got, err := engine.Render(context.Background(), normal, detect.MIMEPDF, 1, describe(72, 0, "png")); err != nil || got.Width != 612 {
		t.Fatalf("the engine after the refused page: %+v, %v", got.Width, err)
	}
}

// A render does not outlive the context it was given, and a page does not
// take longer than a page is given.
func TestARenderStopsWhenItsTimeIsUp(t *testing.T) {
	slow := nestedForms(6)
	// Loading the engine is not part of what is timed.
	if _, err := engine.Count(context.Background(), slow); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := engine.Render(ctx, slow, detect.MIMEPDF, 1, describe(72, 0, "png"))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > time.Second {
		t.Fatalf("a render whose context ended returned after %s with %v", time.Since(began), err)
	}

	engine.Timeout = 100 * time.Millisecond
	defer func() { engine.Timeout = 0 }()
	began = time.Now()
	_, err = engine.Render(context.Background(), slow, detect.MIMEPDF, 1, describe(72, 0, "png"))
	if fault.CodeOf(err) != fault.DocumentCorrupt || !strings.Contains(fault.DetailOf(err), "in the time a page is given") || time.Since(began) > time.Second {
		t.Fatalf("a page past its time returned after %s with %v", time.Since(began), err)
	}
	if _, err := engine.Count(context.Background(), slow); err != nil {
		t.Fatalf("counting does not load a page, so it is quick: %v", err)
	}

	// The instances that were stopped are gone, and the engine has others.
	engine.Timeout = 0
	if got, err := engine.Render(context.Background(), testfixtures.Read(t, testfixtures.TextPDF), detect.MIMEPDF, 1, describe(72, 0, "png")); err != nil || got.Width != 612 {
		t.Fatalf("the engine after two stopped renders: %+v, %v", got.Width, err)
	}
}

// The engine is given no file system. A PDF is a program for the engine,
// and the engine can be asked to open a file by its path.
func TestTheEngineSeesNoFileOfTheHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "on-the-host.pdf")
	if err := os.WriteFile(path, testfixtures.Read(t, testfixtures.TextPDF), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := onInstance(context.Background(), engine, func(instance pdfium.Pdfium) (bool, error) {
		_, err := instance.OpenDocument(&requests.OpenDocument{FilePath: &path})
		return err == nil, nil
	})
	if err != nil || opened {
		t.Fatalf("the engine opened %s from the host: %v, %v", path, opened, err)
	}
}

func TestThePagesOfAPDFAreCountedByTheEngine(t *testing.T) {
	for fixture, want := range map[string]int{testfixtures.TextPDF: 2, testfixtures.MultipagePDF: 3, testfixtures.MinimalPDF: 1} {
		if got, err := engine.Count(context.Background(), testfixtures.Read(t, fixture)); err != nil || got != want {
			t.Errorf("%s: %d pages, %v; want %d", fixture, got, err, want)
		}
	}
	if got, err := (&Pages{PDF: engine}).CountPDF(context.Background(), testfixtures.Read(t, testfixtures.TextPDF)); err != nil || got != 2 {
		t.Errorf("through the renderer of every format: %d, %v", got, err)
	}
	if _, err := engine.Count(context.Background(), []byte("%PDF-1.4\nnothing follows")); fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Errorf("bytes that are no PDF: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Count(ctx, testfixtures.Read(t, testfixtures.TextPDF)); !errors.Is(err, context.Canceled) {
		t.Errorf("a count that was canceled: %v", err)
	}
}

func TestTheEngineIsSharedByAsManyRendersAsItHasInstances(t *testing.T) {
	pdf := testfixtures.Read(t, testfixtures.TextPDF)
	if _, err := engine.Count(context.Background(), pdf); err != nil {
		t.Fatal(err)
	}
	// With every instance taken, a render waits, and stops waiting when
	// its context ends.
	held := make([]*slot, 0, cap(engine.slots))
	for range cap(engine.slots) {
		held = append(held, <-engine.slots)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := engine.Count(ctx, pdf)
	cancel()
	for _, s := range held {
		engine.slots <- s
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a render that found no instance free: %v", err)
	}

	// Work that fails in a way the engine does not report is a failure of
	// the file, and the instance it ran on is not used again.
	_, err = onInstance(context.Background(), engine, func(pdfium.Pdfium) (int, error) { panic("the engine fell over") })
	if fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Fatalf("work that panicked: %v", err)
	}
	if n, err := engine.Count(context.Background(), pdf); err != nil || n != 2 {
		t.Fatalf("the engine afterwards: %d, %v", n, err)
	}

	// An engine whose instances were made ready and never used closes.
	idle := &PDF{Instances: 3}
	idle.once.Do(idle.load)
	if err := idle.Close(); err != nil || cap(idle.slots) != 3 {
		t.Fatalf("closing an engine that rendered nothing: %v", err)
	}
}

// TestMain closes the engine the tests share once they are done: a loaded
// engine closes without an error and gives its instances' memory back.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := engine.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "closing the engine:", err)
		code = 1
	}
	os.Exit(code)
}
