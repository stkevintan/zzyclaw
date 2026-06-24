FROM golang:1.25-alpine3.22 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /zzy .

FROM alpine:3.22

RUN apk add --no-cache \
	ca-certificates \
	font-noto-cjk \
	ripgrep \
	python3 \
	py3-pip && \
	pip install --no-cache-dir --break-system-packages 'markitdown[pdf,docx]'

# Run as an unprivileged user. Root inside a container still shares the host
# kernel, so dropping to a non-root account shrinks the blast radius of the
# agent's shell/script execution tools.
RUN addgroup -S app && adduser -S -G app -h /home/appuser appuser

# markitdown (installed above into the system site-packages) is used by the
# resume extraction feature to convert PDF/DOCX. HOME is set so tools that expect
# a home directory work for the non-root user.
ENV HOME=/home/appuser

COPY --from=builder /zzy /usr/local/bin/zzy

# Deno provides the sandbox that runs user-added (runtime: deno) skills. It is a
# deny-by-default runtime: each skill gets only the read/write paths and network
# hosts the agent grants it per run. The denoland/deno:alpine image ships a
# musl-compatible deno binary, so we copy it straight in. Bump the tag to update.
COPY --from=denoland/deno:alpine-2.1.4 /usr/bin/deno /usr/local/bin/deno

# /app holds the mounted data volume (writable) and the read-only config file.
WORKDIR /app
RUN mkdir -p /app/data && \
	chown -R appuser:app /app /home/appuser

USER appuser
CMD ["zzy"]
