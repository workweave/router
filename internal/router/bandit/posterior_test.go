package bandit

import (
	"path/filepath"
	"testing"
)

func TestLoadPosterior(t *testing.T) {
	path := filepath.Join("testdata", "ts_posterior.json")
	p, err := LoadPosterior(path)
	if err != nil {
		t.Fatal(err)
	}
	sample, ok := p.Sample([]int{0}, "claude-haiku-4-5", func() float64 { return 0 })
	if !ok {
		t.Fatal("expected arm for cluster 0 haiku")
	}
	if sample != 0.8 {
		t.Fatalf("zero-noise sample = mean, got %v", sample)
	}
	_, ok = p.Sample([]int{0}, "missing-model", func() float64 { return 0 })
	if ok {
		t.Fatal("missing arm must not ok")
	}
}

func TestLoadPosterior_MissingFile(t *testing.T) {
	_, err := LoadPosterior(filepath.Join("testdata", "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadPosterior_EmptyCells(t *testing.T) {
	path := filepath.Join("testdata", "empty_posterior.json")
	_, err := LoadPosterior(path)
	if err == nil {
		t.Fatal("expected error for empty posterior")
	}
}
