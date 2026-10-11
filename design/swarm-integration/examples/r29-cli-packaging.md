# R29: Package a verifiable CLI for remote machines

[All examples](README.md) · [Review](../README.md)

The application embeds Linux amd64 and arm64 Dashi binaries. It currently
cross-compiles a pinned private release and defines its own manifest. An upgrade
should let it verify the bytes and protocol it is installing.

## Before: current build code

From [swarm/scripts/build-dashi.sh:10–20](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/scripts/build-dashi.sh#L10). Surrounding definitions are omitted.

<!-- source: swarm scripts/build-dashi.sh 10 20 -->
```sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cache=$(go env GOMODCACHE)
for arch in amd64 arm64; do
  # go install keeps the release's module version in the binary's build
  # info; a cross-built install lands in GOPATH/bin/linux_ARCH.
  env -u GOBIN GOPATH="$work" GOMODCACHE="$cache" GOPRIVATE="${GOPRIVATE:-github.com/deepnoodle-ai/*}" \
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go install -trimpath -ldflags='-s -w' "github.com/deepnoodle-ai/dashi/cmd/dashi@v$version"
  cp "$work/bin/linux_$arch/dashi" "$out/dashi-linux-$arch"
done
```

## After: Proposed release tooling

This release-tool command does not exist. It is separate from the runtime CLI.

```sh
# Proposed Dashi dev/release tool, run by release engineers with a Go toolchain.
go run ./devtools/cli-bundle --version v0.6.0 --target linux/amd64 --target linux/arm64 --out ./bundle
# Binaries + manifest: version, platform, size, hash and control capabilities.
# Swarm retains its loader/verifier and remote installation code.
```

**Observable result:** One release-tool operation would produce the binaries and manifest Swarm needs. A compatible CLI/library pair could still use different SemVer versions.

**Developer benefit:** Remove per-consumer cross-compilation/manifest glue and make upgrade checks predictable.

**Application responsibility:** Pin and verify bytes, control remote installation/caches, and authenticate artifact access. Public distribution remains a business choice. Byte-for-byte reproducibility needs a pinned toolchain/dependency environment and separate verification; a build script alone does not prove it.
