// Package embed provides sentence embedding via hugot's pure-Go backend.
// No CGO or shared libraries are required; the ONNX model is loaded from
// a local cache directory (~/.sieve/models/ by default).
//
// The model is downloaded from HuggingFace Hub on first use and cached on disk.
// Override defaults with:
//
//	SIEVE_EMBED_MODEL  – HuggingFace repo name (default: KnightsAnalytics/all-MiniLM-L6-v2)
//	SIEVE_MODEL_DIR    – local cache dir (default: ~/.sieve/models)
package embed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

const DefaultModel = "KnightsAnalytics/all-MiniLM-L6-v2"

// Embedder wraps a hugot feature-extraction pipeline.
// Create with New or NewWithModel; call Close when done.
type Embedder struct {
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
}

// New creates an Embedder using defaults (or env overrides).
func New(ctx context.Context) (*Embedder, error) {
	model := os.Getenv("SIEVE_EMBED_MODEL")
	if model == "" {
		model = DefaultModel
	}
	dir := os.Getenv("SIEVE_MODEL_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".sieve", "models")
	}
	return NewWithModel(ctx, model, dir)
}

// NewWithModel creates an Embedder with an explicit model name and cache dir.
// The model is downloaded from HuggingFace if not already cached.
func NewWithModel(ctx context.Context, modelName, cacheDir string) (*Embedder, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("embed: create cache dir %s: %w", cacheDir, err)
	}

	session, err := hugot.NewGoSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("embed: hugot go session: %w", err)
	}

	// modelPath: DownloadModel places files in cacheDir/<basename>/
	modelPath := filepath.Join(cacheDir, filepath.Base(modelName))
	if _, statErr := os.Stat(filepath.Join(modelPath, "model.onnx")); os.IsNotExist(statErr) {
		downloaded, dlErr := hugot.DownloadModel(ctx, modelName, cacheDir, hugot.NewDownloadOptions())
		if dlErr != nil {
			session.Destroy()
			return nil, fmt.Errorf("embed: download model %s: %w", modelName, dlErr)
		}
		modelPath = downloaded
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath: modelPath,
		Name:      "sieve",
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		session.Destroy()
		return nil, fmt.Errorf("embed: create pipeline: %w", err)
	}

	return &Embedder{session: session, pipeline: pipeline}, nil
}

// Embed returns one embedding vector per input text.
// Vectors are L2-normalized (hugot's default post-processing).
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out, err := e.pipeline.RunPipeline(ctx, texts)
	if err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}

// EmbedOne is a convenience wrapper for embedding a single text.
func (e *Embedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: no embedding returned")
	}
	return vecs[0], nil
}

// Close releases the underlying hugot session.
func (e *Embedder) Close() {
	if e.session != nil {
		e.session.Destroy()
	}
}
