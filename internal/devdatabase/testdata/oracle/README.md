# Host docker spec oracle

`specs.txt` lists `docker://` dev database specs, one per line. `host.json`
holds the answer of the host's `devdocker.Parse` (`internal/devdocker`) for
each: whether it accepts the spec, and for an accepted one the image and the
address it hands the connector, with the published port 127.0.0.1:12136.

To regenerate, check out the host commit named in `.github/workflows/ci.yml` in
a scratch worktree, add a test to `internal/devdocker` that reads `specs.txt`,
calls `devdocker.Parse` and `Spec.URL("127.0.0.1:12136", "")` for each line and
writes the result in the shape of `host.json`. Remove the scratch worktree
afterwards.
