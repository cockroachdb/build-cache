# build-cache

Build-cache is a small tool to cache the installed output of go builds
(installed via "go install" or "go build -i"). It is useful in CI
scenarios where the CI system attempts to build from scratch on each
run.

Build-cache utilizes the output of "go list -json" to determine the
inputs (.go, .c, etc.) and dependencies for a package and then
constructs a fingerprint (SHA1 digest) from these inputs and the
fingerprints of the dependent packages.

In `save` mode, if the package is considered up to date its installed
output (located in `${GOPATH}/pkg/x/y/z.a`) is copied to the cache
directory and named using the fingerprint of the package.

```
~ build-cache save github.com/cockroachdb/cockroach
saving github.com/cockroachdb/cockroach to /Users/pmattis/buildcache
29c9f6186dd72ec796869ee514d4e8d7847b42e2 *github.com/biogo/store/interval
690d239f64efa2fed909a8f8380393e512eccb67 *github.com/biogo/store/llrb
9ae3c447b434976ddc86e5015cfa47af04946c7e *github.com/cockroachdb/c-lz4
9a879dc243db9715d717e54463c0087565e48897 *github.com/cockroachdb/c-protobuf
11599c2d4688002f7058afc89c6ea606a946f461 *github.com/cockroachdb/c-rocksdb
66be2b3b06539d8d82b4d6eba83f73e0a44a5f91 *github.com/cockroachdb/c-snappy
-                                         github.com/cockroachdb/cockroach
...
```

The `save` output shows the fingerprint of each package and the
package import path. A `*` preceding the import path indicates the
package output was saved to the cache. The absence of `*` indicates
the cache already contained the package output. A `-` for the
fingerprint indicates the package was stale (i.e. the generated output
is not up to date with the source) and that the output was not saved
to the cache directory.

In `restore` mode, the fingerprint of the package is used to lookup
the generated output in the cache directory. If the output exists it
is copied to the package's target.

```
~ build-cache restore github.com/cockroachdb/cockroach
restoring github.com/cockroachdb/cockroach from /Users/pmattis/buildcache
29c9f6186dd72ec796869ee514d4e8d7847b42e2  github.com/biogo/store/interval
690d239f64efa2fed909a8f8380393e512eccb67  github.com/biogo/store/llrb
9ae3c447b434976ddc86e5015cfa47af04946c7e  github.com/cockroachdb/c-lz4
9a879dc243db9715d717e54463c0087565e48897  github.com/cockroachdb/c-protobuf
11599c2d4688002f7058afc89c6ea606a946f461  github.com/cockroachdb/c-rocksdb
66be2b3b06539d8d82b4d6eba83f73e0a44a5f91  github.com/cockroachdb/c-snappy
9a2714cf616d7c4720c9e056de2ad279c2b0477e  github.com/cockroachdb/clog
-                                         github.com/cockroachdb/cockroach
...
```

The `clear` command removes the cache directory and all of its contents.

```
~ build-cache clear
clearing /Users/pmattis/buildcache
```

The cache directory defaults to `${HOME}/buildcache` and can be
overridden using the `CACHE` environment variable.
