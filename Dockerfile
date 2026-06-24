FROM golang:1.25-alpine3.22 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /zzy .

# Runtime uses a glibc base. Deno's official Linux binary is glibc-linked, so a
# glibc image lets it run natively without bundling a separate libc or tweaking
# the loader path for the sandboxed subprocess.
FROM debian:bookworm-slim

# Runtime deps: TLS roots, CJK fonts, ripgrep (search_files tool) and python3 +
# markitdown (the resume extraction feature converts PDF/DOCX). curl plus a few
# common CLI utilities are handy for the agent's shell tool and debugging.
RUN apt-get update && apt-get install -y --no-install-recommends \
	ca-certificates \
	fonts-noto-cjk \
	ripgrep \
	curl \
	wget \
	jq \
	git \
	less \
	procps \
	python3 \
	python3-pip && \
	pip install --no-cache-dir --break-system-packages 'markitdown[pdf,docx]' && \
	ln -sf /usr/bin/python3 /usr/local/bin/python && \
	rm -rf /var/lib/apt/lists/*

# Run as an unprivileged user. Root inside a container still shares the host
# kernel, so dropping to a non-root account shrinks the blast radius of the
# agent's shell/script execution tools.
RUN groupadd -r app && useradd -r -g app -m -d /home/appuser appuser

# markitdown (installed above into the system site-packages) is used by the
# resume extraction feature to convert PDF/DOCX. HOME is set so tools that expect
# a home directory work for the non-root user.
ENV HOME=/home/appuser

COPY --from=builder /zzy /usr/local/bin/zzy

# Deno provides the sandbox that runs user-added (runtime: deno) skills. It is a
# deny-by-default runtime: each skill gets only the read/write paths and network
# hosts the agent grants it per run. denoland/deno:bin is a multi-arch image
# holding just the deno binary; on this glibc base it runs as-is. Bump to update.
COPY --from=denoland/deno:bin-2.1.4 /deno /usr/local/bin/deno

# /app holds the mounted data volume (writable) and the read-only config file.
# Pre-create /app/data owned by appuser so a fresh named volume mounted there
# inherits writable ownership.
WORKDIR /app
RUN mkdir -p /app/data && \
	chown -R appuser:app /app /home/appuser

USER appuser
CMD ["zzy"]
