package proto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// docs/testvectors.json exists so another implementation (the Flutter client)
// can be checked against this one byte for byte. It is only worth anything if
// it matches what the Go encoder actually produces, so this test holds it to
// that. Run with -update to regenerate after a deliberate wire change.
var update = os.Getenv("UPDATE_VECTORS") != ""

const vectorsPath = "../../docs/testvectors.json"

type vector struct {
	Input json.RawMessage `json:"input"`
	JSON  string          `json:"json"`
}

type vectorFile struct {
	Note        string            `json:"_note"`
	Hello       map[string]vector `json:"hello"`
	Envelopes   map[string]vector `json:"envelopes"`
	Rejection   map[string]any    `json:"rejection"`
	Negotiation map[string]any    `json:"version_negotiation"`
	Frame       map[string]any    `json:"frame"`
}

func TestProtocolTestVectorsMatchTheEncoder(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var f vectorFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Envelopes) == 0 {
		t.Fatal("no envelope vectors")
	}

	changed := false
	for name, v := range f.Envelopes {
		var e Envelope
		if err := json.Unmarshal(v.Input, &e); err != nil {
			t.Errorf("%s: input is not an envelope: %v", name, err)
			continue
		}
		got, err := Encode(e)
		if err != nil {
			t.Errorf("%s: the documented vector does not encode: %v", name, err)
			continue
		}
		if string(got) != v.JSON {
			if update {
				v.JSON = string(got)
				f.Envelopes[name] = v
				changed = true
				continue
			}
			t.Errorf("%s:\n  vector:  %s\n  encoder: %s", name, v.JSON, got)
		}
		// And the documented bytes must decode back to the same envelope.
		back, err := Decode([]byte(v.JSON))
		if err != nil {
			if !update {
				t.Errorf("%s: the documented json does not decode: %v", name, err)
			}
			continue
		}
		again, err := Encode(back)
		if err != nil || string(again) != v.JSON {
			t.Errorf("%s: round trip changed the envelope: %s", name, again)
		}
	}

	for name, v := range f.Hello {
		var hl Hello
		if err := json.Unmarshal(v.Input, &hl); err != nil {
			t.Errorf("hello %s: %v", name, err)
			continue
		}
		got, err := json.Marshal(hl)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != v.JSON {
			if update {
				v.JSON = string(got)
				f.Hello[name] = v
				changed = true
				continue
			}
			t.Errorf("hello %s:\n  vector:  %s\n  encoder: %s", name, v.JSON, got)
		}
	}

	if update && changed {
		out, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Clean(vectorsPath), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("vectors regenerated")
	}
}

// The rejection vectors document inputs every implementation must refuse.
func TestRejectionVectorsAreRejected(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Rejection map[string]json.RawMessage `json:"rejection"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Rejection) == 0 {
		t.Fatal("no rejection vectors")
	}
	checked := 0
	for name, raw := range f.Rejection {
		var v struct {
			JSON   string `json:"json"`
			Reason string `json:"reason"`
		}
		// Some entries describe a structural case (an empty frame) rather than
		// a decodable payload; those carry no json field to try.
		if err := json.Unmarshal(raw, &v); err != nil || v.JSON == "" {
			continue
		}
		checked++
		if _, err := Decode([]byte(v.JSON)); err == nil {
			t.Errorf("%s: %q was accepted, but the vector says it must be rejected (%s)", name, v.JSON, v.Reason)
		}
	}
	if checked == 0 {
		t.Fatal("no rejection vector carried a payload to try")
	}
}

// The constants the vector file publishes must be the constants in the code.
func TestFrameConstantsMatchTheVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Frame map[string]json.RawMessage `json:"frame"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{
		"max_frame": MaxFrame, "max_body": MaxBody,
		"max_file_size": MaxFileSize, "id_len_bytes": IDLen, "chunk_size": ChunkSize,
	} {
		raw, ok := f.Frame[name]
		if !ok {
			t.Errorf("vectors do not document %s", name)
			continue
		}
		var got int64
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("%s: not a number in the vectors: %s", name, raw)
			continue
		}
		if got != want {
			t.Errorf("%s: vectors say %d, code says %d", name, got, want)
		}
	}
}
