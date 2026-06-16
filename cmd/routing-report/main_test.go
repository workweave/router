package main

import (
	"testing"
)

// Paths relative to this test's working directory (cmd/routing-report).
const (
	testCorpus    = "../../internal/router/cluster/testdata/register_probes.jsonl"
	testEmb       = "../../internal/router/cluster/testdata/register_probes.emb"
	testArtifacts = "../../internal/router/cluster/artifacts"
)

// TestEmbeddingCacheFresh is the guard that keeps the committed probe
// embeddings in sync with the corpus. If someone edits register_probes.jsonl
// without rerunning scripts/embed_register_probes.py, the cached vectors are
// stale and every routing report would be silently wrong. Pure file IO — no
// ONNX, runs in default CI.
func TestEmbeddingCacheFresh(t *testing.T) {
	probes, corpusSHA, err := loadCorpus(testCorpus)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	hdr, vecs, err := loadEmb(testEmb)
	if err != nil {
		t.Fatalf("load emb: %v", err)
	}
	if hdr.CorpusSHA256 != corpusSHA {
		t.Fatalf("STALE embedding cache: %s built for corpus sha %s, corpus is now %s.\n"+
			"Run: python scripts/embed_register_probes.py", testEmb, hdr.CorpusSHA256, corpusSHA)
	}
	if hdr.N != len(probes) || len(vecs) != len(probes) {
		t.Fatalf("cache size mismatch: header N=%d vecs=%d corpus=%d", hdr.N, len(vecs), len(probes))
	}
	for i, v := range vecs {
		if len(v) != hdr.EmbedDim {
			t.Fatalf("probe %d vector dim %d != header dim %d", i, len(v), hdr.EmbedDim)
		}
	}
}

// TestReportRunsOnLatest exercises the full report path (real Scorer fed by the
// static embedder) against the currently-deployed `latest` artifact, asserting
// every probe routes to a concrete model. Catches an artifact that fails to
// load or leaves a probe unrouted.
func TestReportRunsOnLatest(t *testing.T) {
	latest, err := readLatest(testArtifacts)
	if err != nil {
		t.Fatalf("read latest: %v", err)
	}
	probes, _, err := loadCorpus(testCorpus)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	hdr, vecs, err := loadEmb(testEmb)
	if err != nil {
		t.Fatalf("load emb: %v", err)
	}
	emb := buildEmbedder(hdr, probes, vecs)

	res, err := routeCorpus(testArtifacts, latest, probes, emb, 2)
	if err != nil {
		t.Fatalf("route corpus on %s: %v", latest, err)
	}
	for i, m := range res.chosen {
		if m == "" {
			t.Fatalf("probe %s (%s) routed to empty model", probes[i].ID, probes[i].Register)
		}
	}
}
