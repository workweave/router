"""Files shared between download_from_hf.py and the Dockerfile.

Each entry maps the *local* filename (what hugot loads from
`internal/router/cluster/assets/`) to the *HF* path (where the file
lives in the upstream HF repo). Most entries are identity, but the
quantized ONNX is at `onnx/model_quantized.onnx` upstream and we
prefer to write it as `model.onnx` locally — that name is also what
hugot's pipeline config asks for.

REQUIRED files fail the build/download on miss; OPTIONAL files are
HF transformers-convention companions that hugot doesn't need at
runtime (only useful for AutoTokenizer parity testing from Python).
"""

# (local_name, hf_path)
REQUIRED_FILES = (
    ("model.onnx", "onnx/model_quantized.onnx"),
    ("tokenizer.json", "tokenizer.json"),
)

OPTIONAL_FILES = (
    ("config.json", "config.json"),
    ("tokenizer_config.json", "tokenizer_config.json"),
    ("special_tokens_map.json", "special_tokens_map.json"),
)
