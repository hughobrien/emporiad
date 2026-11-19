test:
    go test ./...

race:
    go test -race ./...

build:
    go build -o emporiad ./cmd/emporiad

# Run the hermetic CI gate on bigthink (the Mac has no nix).
gate:
    rsync -a --delete --exclude .git ./ bigthink:~/emporiad-gate/
    ssh bigthink 'mkdir -p /tmp/egate && cd ~/emporiad-gate && TMPDIR=/tmp/egate nix build --no-write-lock-file -L "path:$PWD#checks.x86_64-linux.gate" > /tmp/egate.log 2>&1; echo "NIX EXIT=$?"; tail -30 /tmp/egate.log'
