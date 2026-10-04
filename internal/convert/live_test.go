// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lectio/document"
	"latere.ai/x/lectio/internal/fault"
	"latere.ai/x/lectio/internal/intake/detect"
	"latere.ai/x/lectio/internal/intake/pages"
	"latere.ai/x/lectio/internal/parse"
	"latere.ai/x/lectio/internal/render"
	"latere.ai/x/lectio/internal/testfixtures"
)

// TestLiveConverter takes every fixture that needs conversion through a
// running sidecar, and so through a real office suite: the pipeline
// detects the file, has the sidecar convert it, and reads the conversion
// the way a parse does. It needs a sidecar, so it is never part of the
// gate:
//
//	LECTIO_LIVE_CONVERTER=http://127.0.0.1:8090 make live-convert
//
// LECTIO_LIVE_OUT is a directory each conversion is written to.
func TestLiveConverter(t *testing.T) {
	url := os.Getenv("LECTIO_LIVE_CONVERTER")
	if url == "" {
		t.Skip("LECTIO_LIVE_CONVERTER names no sidecar")
	}
	c, err := New(Config{URL: url, MaxBytes: pages.DefaultLimits().MaxBytes})
	if err != nil {
		t.Fatal(err)
	}
	p := &parse.Pipeline{Limits: pages.DefaultLimits(), Renderer: render.NewPages(), Converter: c}

	for _, fixture := range converted {
		t.Run(filepath.Ext(fixture), func(t *testing.T) {
			started := time.Now()
			got, err := p.Prepare(context.Background(), testfixtures.Read(t, fixture), detect.DeclaredType{FileName: fixture}, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d bytes of %s, %d pages, in %s", fixture, len(got.Working), got.Manifest.MediaType, got.Manifest.PagesTotal, time.Since(started).Round(time.Millisecond))
			if dir := os.Getenv("LECTIO_LIVE_OUT"); dir != "" {
				name := strings.TrimSuffix(filepath.Base(fixture), filepath.Ext(fixture)) + filepath.Ext(fixture) + map[string]string{detect.MIMEPDF: ".pdf", detect.MIMEDOCX: ".docx"}[got.Manifest.MediaType]
				if err := os.WriteFile(filepath.Join(dir, name), got.Working, 0o600); err != nil {
					t.Error(err)
				}
			}

			if fixture == testfixtures.DOC {
				// The conversion is a document read from its own structure.
				var text []string
				for _, b := range got.Native[0].Blocks {
					text = append(text, b.Text)
				}
				if got.Manifest.MediaType != detect.MIMEDOCX || got.Manifest.Source != document.SourceNative || !strings.Contains(strings.Join(text, "\n"), "block quotes") {
					t.Fatalf("manifest %+v, text %q", got.Manifest, text)
				}
				return
			}
			// The conversion is a PDF the engine opens and counts.
			if got.Manifest.MediaType != detect.MIMEPDF || got.Manifest.Source != document.SourceReader || got.Manifest.PagesTotal < 1 || !bytes.HasPrefix(got.Working, []byte("%PDF-")) {
				t.Fatalf("manifest %+v", got.Manifest)
			}
		})
	}

	// A file that is not what it is sent as is the file's fault, and the
	// sidecar converts the next one.
	if _, err := c.Convert(context.Background(), []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1 and nothing a word processor wrote"), detect.MIMEDOC, detect.MIMEDOCX); fault.CodeOf(err) != fault.DocumentCorrupt {
		t.Errorf("a file the suite cannot load: %v", err)
	}
	if _, err := c.Convert(context.Background(), testfixtures.Read(t, testfixtures.RTF), detect.MIMERTF, detect.MIMEPDF); err != nil {
		t.Errorf("the conversion after a failed one: %v", err)
	}
}
