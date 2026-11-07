# Lectio on one machine

For someone trying Lectio: this page runs the server on one machine, with
every dependency it needs, from a clone of the repository, and ends in
requests that show it working. It first runs the development server,
which keeps everything in memory, and then the durable stack, which
keeps a parse across a restart. Installing Lectio for other people is
[install.md](install.md).

Run the blocks in order, in one shell, from the root of a clone. Each one
assumes the ones before it.

## What to install

| Tool | For |
|---|---|
| Go, the version `go.mod` names | building `lectiod` |
| `make`, `git`, `curl` | the build, the clone, the requests |
| `podman` with `podman compose` | the durable stack and the conversion sidecar |

The development server needs Go and `curl` and no container.

## The development server

One process that keeps files, parses and results in memory, takes the
token `dev` (`LECTIO_DEV_TOKEN` names another), and reads pages with a
stub reader that calls no model. Nothing survives a stop. It listens on
`127.0.0.1:8080`, the loopback address alone, unless `LECTIO_ADDR` says
otherwise:

```sh
make build
mkdir -p out
LECTIO_DEV=true LECTIO_ADDR=127.0.0.1:8080 out/lectiod > out/dev.log 2>&1 &
echo $! > out/dev.pid
until curl -sf http://127.0.0.1:8080/v1/openapi.yaml > /dev/null; do sleep 1; done
```

Make a small file in the shell and upload it. The answer holds the
file's `id`, which a parse names. The same bytes uploaded twice are one
file:

```sh
api=http://127.0.0.1:8080/v1
auth='Authorization: Bearer dev'
printf 'name,qty\nbolts,12\nnuts,40\n' > out/sample.csv

curl -s -H "$auth" --data-binary @out/sample.csv "$api/files?name=sample.csv" | tee out/file.json
echo
file=$(sed -n 's/.*"id":"\(fil_[^"]*\)".*/\1/p' out/file.json)
```

Parse it. `Prefer: wait=30` holds the answer up to 30 seconds for the
parse to end:

```sh
curl -s -H "$auth" -H 'Prefer: wait=30' -H 'Content-Type: application/json' \
  -d "{\"source\":{\"file\":\"$file\"}}" "$api/parses" | tee out/parse.json
echo
parse=$(sed -n 's/^{"id":"\(prs_[^"]*\)".*/\1/p' out/parse.json)
```

The parse is `succeeded`. Read the result back, as Markdown and as one
page with its blocks:

```sh
curl -s -H "$auth" "$api/parses/$parse/document?format=markdown"
curl -s -H "$auth" "$api/parses/$parse/pages/1"
```

The first prints the file as a table. The second prints the page as
JSON, with the same table as a block and its cells:

```text
| name | qty |
| --- | --- |
| bolts | 12 |
| nuts | 40 |
```

A CSV is read from the file itself. A PDF or an image goes to a reader,
which here is the stub: it calls no model and describes the page it was
given. [api.md](api.md) has every route and the other ways to read a
result.

Stop it:

```sh
kill "$(cat out/dev.pid)"
```

## The durable stack on one machine

`deploy/examples/compose.yaml` runs Postgres for the parses and their
tasks, an S3-compatible object store for the bytes, `lectiod` in the role
`all`, which is the API and a worker in one process, and the conversion
sidecar on a socket with no network. Only `lectiod`'s port is published,
on the loopback interface, and every credential in the file is an
example.

### The issuer

The durable server takes no static token. It verifies each caller's
bearer against an OpenID Connect issuer, for the audience `lectio`, and
does not start without one. On one machine there is none, so this block
writes a small one that gives a token to anyone who asks. It is for this
machine alone: the stack below publishes it on the loopback interface
only.

```sh
mkdir -p out/issuer
printf 'module issuer\n\ngo 1.22\n' > out/issuer/go.mod
cat > out/issuer/main.go <<'GO'
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":8099", "address to listen on")
	issuer := flag.String("issuer", "http://issuer:8099", "the issuer URL, as lectiod reaches it")
	flag.Parse()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return b64(b)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": *issuer, "jwks_uri": *issuer + "/jwks.json"})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "local",
			"x": b64(key.X.FillBytes(make([]byte, 32))), "y": b64(key.Y.FillBytes(make([]byte, 32))),
		}}})
	})
	// GET /token?sub=alice answers a token for the subject alice.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		sub := r.URL.Query().Get("sub")
		if sub == "" {
			http.Error(w, "name a subject: /token?sub=alice", http.StatusBadRequest)
			return
		}
		now := time.Now()
		in := enc(map[string]any{"alg": "ES256", "typ": "JWT", "kid": "local"}) + "." +
			enc(map[string]any{"iss": *issuer, "sub": sub, "aud": "lectio", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
		sum := sha256.Sum256([]byte(in))
		r1, s1, err := ecdsa.Sign(rand.Reader, key, sum[:])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sig := make([]byte, 64)
		r1.FillBytes(sig[:32])
		s1.FillBytes(sig[32:])
		w.Write([]byte(in + "." + b64(sig)))
	})
	log.Printf("issuer %s listening on %s", *issuer, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
GO
```

A caller is the `sub` of its token at this issuer, and owns the files
and the parses it creates: nobody else reads or changes them.

### The stack

This second file runs the issuer as a service of the stack:

```sh
cat > out/stack.yaml <<'EOF'
services:
  issuer:
    image: docker.io/library/golang:1.27
    working_dir: /src
    command: ["go", "run", ".", "-addr", ":8099", "-issuer", "http://issuer:8099"]
    environment:
      GOCACHE: /tmp/gocache
      GOFLAGS: -buildvcs=false
    volumes:
      - ../../out/issuer:/src:ro,z
    ports:
      - "127.0.0.1:8099:8099"
EOF
```

`lectiod` does not start without `LECTIO_OIDC_ISSUERS`, and the command
stops and says so. The function below names the issuer for every
`compose` command of this page. Start the stack with it. The first
start builds the image of `lectiod` and of the sidecar, which takes a few
minutes:

```sh
compose() {
  LECTIO_OIDC_ISSUERS=http://issuer:8099 \
    podman compose -f deploy/examples/compose.yaml -f out/stack.yaml "$@"
}
compose up -d --build
until curl -sf http://127.0.0.1:8080/v1/openapi.yaml > /dev/null; do sleep 1; done
until curl -sf 'http://127.0.0.1:8099/token?sub=alice' > /dev/null; do sleep 1; done
```

### A caller, and a parse that survives a restart

A caller gets a token from the issuer and sends it as the bearer. The
audience is `lectio`, which is what `LECTIO_OIDC_AUDIENCE` defaults to.
Upload the file and parse it as before:

```sh
api=http://127.0.0.1:8080/v1
auth="Authorization: Bearer $(curl -s 'http://127.0.0.1:8099/token?sub=alice')"

file=$(curl -s -H "$auth" --data-binary @out/sample.csv "$api/files?name=sample.csv" \
  | sed -n 's/.*"id":"\(fil_[^"]*\)".*/\1/p')
curl -s -H "$auth" -H 'Prefer: wait=30' -H 'Content-Type: application/json' \
  -d "{\"source\":{\"file\":\"$file\"}}" "$api/parses" | tee out/parse.json
echo
parse=$(sed -n 's/^{"id":"\(prs_[^"]*\)".*/\1/p' out/parse.json)
curl -s -H "$auth" "$api/parses/$parse/document?format=markdown"
```

The `owner` of the parse is the issuer and the `sub`, joined by `|`. A
token for another subject does not find it:

```sh
curl -s -H "Authorization: Bearer $(curl -s 'http://127.0.0.1:8099/token?sub=bob')" \
  "$api/parses/$parse"
```

Restart `lectiod` and read the parse again. The parse and its document
are in Postgres and the object store, and the new process reads them
from there:

```sh
compose restart lectiod
until curl -sf http://127.0.0.1:8080/v1/openapi.yaml > /dev/null; do sleep 1; done
curl -s -H "$auth" "$api/parses/$parse/document?format=markdown"
```

The table prints again, from the new process. How long a file and a parse are kept is in
[configuration.md](configuration.md). To let some subjects read
everyone's parses, or to have a service of your own decide what a caller
may do and with what limits, see [authorizer.md](authorizer.md).

## Office files and the conversion sidecar

Presentations, rich text, open document text and legacy Word files are
converted to pages first, by a sidecar that holds an office suite. The
stack above runs it with no network, on a socket it shares with
`lectiod`. A rich text file is the smallest office file to make in the
shell:

```sh
printf '{\\rtf1\\ansi\\deff0 {\\b Quarterly report}\\par Sales rose in March.\\par}' > out/note.rtf
file=$(curl -s -H "$auth" --data-binary @out/note.rtf "$api/files?name=note.rtf" \
  | sed -n 's/.*"id":"\(fil_[^"]*\)".*/\1/p')
curl -s -H "$auth" -H 'Prefer: wait=60' -H 'Content-Type: application/json' \
  -d "{\"source\":{\"file\":\"$file\"}}" "$api/parses" | tee out/note.json
echo
note=$(sed -n 's/^{"id":"\(prs_[^"]*\)".*/\1/p' out/note.json)
curl -s -H "$auth" "$api/parses/$note/document?format=markdown"
```

The page the sidecar made is an image, which the stub reader describes by
its size. A model reads it as a page.

The development server reaches the sidecar over a port instead of a
socket. The container then has a network, which is for your own files and
not for ones you do not trust:

```sh
podman build -f deploy/converter/Dockerfile -t lectio-convert .
podman run -d --name lectio-convert-laptop -p 127.0.0.1:8090:8090 lectio-convert
LECTIO_DEV=true LECTIO_ADDR=127.0.0.1:8083 LECTIO_CONVERTER_URL=http://127.0.0.1:8090 \
  out/lectiod > out/dev-convert.log 2>&1 &
echo $! > out/dev-convert.pid
until curl -sf http://127.0.0.1:8083/v1/openapi.yaml > /dev/null; do sleep 1; done

dev='Authorization: Bearer dev'
file=$(curl -s -H "$dev" --data-binary @out/note.rtf "http://127.0.0.1:8083/v1/files?name=note.rtf" \
  | sed -n 's/.*"id":"\(fil_[^"]*\)".*/\1/p')
curl -s -H "$dev" -H 'Prefer: wait=60' -H 'Content-Type: application/json' \
  -d "{\"source\":{\"file\":\"$file\"}}" "http://127.0.0.1:8083/v1/parses" | head -c 160
echo
kill "$(cat out/dev-convert.pid)"
podman rm -f lectio-convert-laptop
```

## Reading pages with a model

Neither server above calls a model. To read a PDF or an image with one,
write a Reader document and a Policy that names it, and set
`LECTIO_CONFIG` to the file's path. The minimal pair, for any endpoint
that speaks the OpenAI chat completions API:

```yaml
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1
  model: your-vision-model
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read: { chain: [default] }
```

The key the endpoint takes is `LECTIO_MODEL_KEY`. No document holds a
key. For the stack above, mount the document into `lectiod` with a third
compose file and pass the key through the environment. The block below
uses the example endpoint and a placeholder key, so it shows the server
starting with the reader and not a page being read; put your endpoint
and your key in their places to read one:

```sh
cat > out/readers.yaml <<'EOF'
apiVersion: lectio.latere.ai/v1
kind: Reader
metadata: { name: default }
spec:
  adapter: chat
  endpoint: https://gateway.example/v1
  model: your-vision-model
---
apiVersion: lectio.latere.ai/v1
kind: Policy
metadata: { name: default }
spec:
  read: { chain: [default] }
EOF
cat > out/models.yaml <<'EOF'
services:
  lectiod:
    environment:
      LECTIO_CONFIG: /etc/lectio/readers.yaml
      LECTIO_MODEL_KEY: ${LECTIO_MODEL_KEY:?set the key your endpoint takes}
    volumes:
      - ../../out/readers.yaml:/etc/lectio/readers.yaml:ro,z
EOF
export LECTIO_MODEL_KEY=placeholder-not-a-key
compose() {
  LECTIO_OIDC_ISSUERS=http://issuer:8099 \
    podman compose -f deploy/examples/compose.yaml -f out/stack.yaml -f out/models.yaml "$@"
}
compose up -d lectiod
until curl -sf http://127.0.0.1:8080/v1/openapi.yaml > /dev/null; do sleep 1; done
curl -s -H "$auth" "$api/readers"
```

[readers.md](readers.md) has every field of a Reader and a Policy, the
`text` adapter that reads a PDF's own text with no model, and how a
request names a reader. [configuration.md](configuration.md) has every
setting, including a key per tenant in place of one for every caller.

## Stop and clean up

This removes everything the page started that still runs: the containers
and the volumes of the stack. The development servers were stopped where
the page started them.

```sh
compose down --volumes
podman rm -f lectio-convert-laptop
unset LECTIO_MODEL_KEY
```

The images it built, `lectio-lectiod` and `lectio-lectio-convert` of the
stack and `lectio-convert` of the sidecar, stay as a cache for the next
start, and `podman rmi` removes them. The files the page wrote are under
`out/`, which Git ignores.
