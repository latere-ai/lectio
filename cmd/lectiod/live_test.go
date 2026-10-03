// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveReader parses a real file with a real reader, end to end: the
// server as it ships, the file through intake and rendering, every page
// through the configured model, and the result read back. It calls a model,
// so it runs only when asked:
//
//	LECTIO_LIVE_CONFIG=reader.yaml LECTIO_LIVE_FILE=paper.pdf make live
//
// LECTIO_LIVE_CONFIG is a Reader document as LECTIO_CONFIG takes it.
// LECTIO_LIVE_PAGES selects pages (default "1-3"), LECTIO_MODEL_KEY is the
// key the reader is called with, and LECTIO_LIVE_OUT, when set, is a
// directory the Markdown and each page's blocks are written to for a
// person to read.
//
// With LECTIO_LIVE_DESCRIBE set, the figures of the parse are then
// described by the describe chain of the configuration's Policy, each
// figure that has a box must come back with a description, and the
// listing and each figure's image are written to LECTIO_LIVE_OUT.
func TestLiveReader(t *testing.T) {
	config, file := os.Getenv("LECTIO_LIVE_CONFIG"), os.Getenv("LECTIO_LIVE_FILE")
	if config == "" || file == "" {
		t.Skip("set LECTIO_LIVE_CONFIG and LECTIO_LIVE_FILE to read a file with a real model")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	selection := os.Getenv("LECTIO_LIVE_PAGES")
	if selection == "" {
		selection = "1-3"
	}

	base, _, _ := started(t, env(
		"LECTIO_DEV", "true", "LECTIO_ADDR", "127.0.0.1:0", "LECTIO_CONFIG", config,
		"LECTIO_MODEL_KEY", os.Getenv("LECTIO_MODEL_KEY"), "LECTIO_WORKERS", "2",
	))
	status, uploaded, raw := call(t, "POST", base+"/v1/files?name="+filepath.Base(file), "dev", data)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("upload: %d %s", status, raw)
	}
	submit, _ := json.Marshal(map[string]any{"source": map[string]string{"file": uploaded["id"].(string)}, "pages": selection})
	status, parse, raw := call(t, "POST", base+"/v1/parses", "dev", submit)
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("submit: %d %s", status, raw)
	}
	at := base + "/v1/parses/" + parse["id"].(string)

	began := time.Now()
	for parse["state"] == "queued" || parse["state"] == "running" {
		if time.Since(began) > 30*time.Minute {
			t.Fatalf("the parse did not end in 30 minutes: %v", parse["progress"])
		}
		time.Sleep(2 * time.Second)
		_, parse, _ = call(t, "GET", at, "dev", nil)
	}
	progress := parse["progress"].(map[string]any)
	t.Logf("%s: %v pages in %s, usage %v", parse["state"], progress["pages_total"], time.Since(began).Round(time.Second), parse["usage"])
	if parse["state"] != "succeeded" {
		t.Fatalf("the parse ended %s: %v", parse["state"], parse["error"])
	}

	out := os.Getenv("LECTIO_LIVE_OUT")
	_, listed, _ := call(t, "GET", at+"/pages", "dev", nil)
	for _, summary := range listed["pages"].([]any) {
		n := int(summary.(map[string]any)["number"].(float64))
		_, page, raw := call(t, "GET", at+"/pages/"+itoa(n), "dev", nil)
		blocks := page["blocks"].([]any)
		if page["model"] == "" || len(blocks) == 0 {
			t.Errorf("page %d: model %v, %d blocks", n, page["model"], len(blocks))
		}
		for _, b := range blocks {
			block := b.(map[string]any)
			// A box, when a reader gives one, lies on the page and has an area.
			if box, ok := block["box"].([]any); ok {
				x0, y0, x1, y1 := box[0].(float64), box[1].(float64), box[2].(float64), box[3].(float64)
				if x0 < 0 || y0 < 0 || x1 > 1 || y1 > 1 || x0 >= x1 || y0 >= y1 {
					t.Errorf("page %d block %v: box %v", n, block["ref"], box)
				}
			}
		}
		if status, _, img := call(t, "GET", at+"/pages/"+itoa(n)+"/image", "dev", nil); status != http.StatusOK || len(img) == 0 {
			t.Errorf("page %d has no image: %d", n, status)
		} else if out != "" {
			write(t, filepath.Join(out, "page-"+itoa(n)+".png"), img)
		}
		if out != "" {
			write(t, filepath.Join(out, "page-"+itoa(n)+".json"), raw)
		}
	}
	if os.Getenv("LECTIO_LIVE_DESCRIBE") != "" {
		describeFigures(t, at, out)
	}

	status, _, markdown := call(t, "GET", at+"/document?format=markdown", "dev", nil)
	if status != http.StatusOK || len(strings.TrimSpace(string(markdown))) == 0 {
		t.Fatalf("the document as Markdown: %d, %d bytes", status, len(markdown))
	}
	if out != "" {
		write(t, filepath.Join(out, "document.md"), markdown)
		t.Logf("wrote the result to %s", out)
	}
}

// describeFigures has the figures of the parse at a URL described, waits
// for the run, and checks that every figure that could be cut from its
// page came back with a description.
func describeFigures(t *testing.T, at, out string) {
	t.Helper()
	status, listing, raw := call(t, "POST", at+"/figures", "dev", nil)
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("describe figures: %d %s", status, raw)
	}
	began := time.Now()
	for {
		run, _ := listing["run"].(map[string]any)
		if run["state"] != "running" {
			t.Logf("figures %s: %v of %v described in %s, usage %v", run["state"], run["done"], run["total"], time.Since(began).Round(time.Second), run["usage"])
			break
		}
		if time.Since(began) > 20*time.Minute {
			t.Fatalf("the figures were not described in 20 minutes: %v", run)
		}
		time.Sleep(2 * time.Second)
		_, listing, raw = call(t, "GET", at+"/figures", "dev", nil)
	}
	for _, f := range listing["figures"].([]any) {
		figure := f.(map[string]any)
		ref := figure["ref"].(string)
		if figure["box"] == nil {
			continue
		}
		if figure["description"] == nil || figure["description"] == "" {
			t.Errorf("figure %s has no description: %v", ref, figure["error"])
		}
		t.Logf("figure %s (%v): %v", ref, figure["figure"], figure["description"])
		if status, _, img := call(t, "GET", at+"/blocks/"+ref+"/image", "dev", nil); status != http.StatusOK || len(img) == 0 {
			t.Errorf("figure %s has no image: %d", ref, status)
		} else if out != "" {
			write(t, filepath.Join(out, "figure-"+ref+".png"), img)
		}
	}
	if out != "" {
		write(t, filepath.Join(out, "figures.json"), raw)
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
