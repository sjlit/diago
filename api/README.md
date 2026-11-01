# Frozen public API

`v2.api` is the [apidiff](https://pkg.go.dev/golang.org/x/exp/cmd/apidiff)
export-data baseline for the public API of the library module
(`github.com/sjlit/diago/v2`, including `audio`, `media` and `media/sdp`).
`examples` is a separate Go module and is intentionally not part of this
baseline.

It is not human-editable. CI runs `scripts/api-check.sh` and fails when the
current public API differs from this file in any way.

## When the API changes intentionally

1. Make the code change.
2. Regenerate the baseline:

   ```sh
   scripts/api-freeze.sh
   ```

3. Review the resulting `api/v2.api` change, commit it together with the code,
   and mention the API change in the commit message.

## Ad-hoc comparison

To compare against an arbitrary git ref without touching the frozen baseline:

```sh
scripts/apidiff.sh <git-ref>
```
