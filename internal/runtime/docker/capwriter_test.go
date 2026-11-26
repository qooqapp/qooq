package docker

import (
	"bytes"
	"testing"
)

// capWriter keeps the first max bytes and still accepts everything, so the
// exec stream is drained to the end however long the output is.
func TestCapWriterKeepsTheStartAndDrainsTheRest(t *testing.T) {
	var buf bytes.Buffer
	w := &capWriter{buf: &buf, max: 5}
	for _, chunk := range []string{"abc", "defg", "hij"} {
		if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; want every byte accepted", chunk, n, err)
		}
	}
	if buf.String() != "abcde" {
		t.Errorf("kept %q, want the first 5 bytes", buf.String())
	}
}
