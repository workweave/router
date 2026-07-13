FROM python:3.12-slim AS build

ENV POETRY_VERSION=1.8.4 \
    POETRY_VIRTUALENVS_IN_PROJECT=true
WORKDIR /app
RUN pip install --no-cache-dir poetry==${POETRY_VERSION}
COPY sidecars/hmm/pyproject.toml sidecars/hmm/poetry.lock ./
RUN poetry install --only=main --no-root --no-interaction

FROM python:3.12-slim

ARG VERSION=dev
ENV VERSION=${VERSION} \
    PATH=/app/.venv/bin:${PATH} \
    PYTHONPATH=/app \
    OMP_NUM_THREADS=1 \
    OPENBLAS_NUM_THREADS=1 \
    MKL_NUM_THREADS=1 \
    NUMEXPR_NUM_THREADS=1 \
    PORT=8093
WORKDIR /app
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --no-create-home --shell /usr/sbin/nologin sidecar
COPY --from=build /app/.venv .venv
COPY sidecars/hmm/hmm_sidecar hmm_sidecar
USER sidecar
EXPOSE 8093
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD curl --fail http://127.0.0.1:${PORT}/livez || exit 1
CMD ["sh", "-c", "exec uvicorn hmm_sidecar.api:app --host 0.0.0.0 --port ${PORT}"]
