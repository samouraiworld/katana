# Regenerating the GovDAO render fixtures

`../render/*.md` are verbatim `gno.land/r/gov/dao:<id>` pages produced by the
GovDAO v3 realm itself, not hand-written strings. They were generated from
gno commit `2c817cec4` with the two filetests in this directory
(`*.gno.txt`, renamed so Go tooling ignores them).

1. Build a `gno` binary from the same gno checkout as the realm source. An
   older installed binary fails on stdlib natives:

   ```sh
   cd <gno> && go build -o /tmp/gno ./gnovm/cmd/gno
   ```

2. `gno test` only resolves `gno.land/...` imports for packages inside the
   examples tree, so work on a scratch copy of `GNOROOT` (symlink everything
   except `examples/`, which you copy). Then drop each filetest into its own
   package with a `gnomod.toml`:

   ```sh
   mkdir -p $ROOT/examples/gno.land/r/test/katanafixtures/frac
   cp z_renders_filetest.gno.txt $ROOT/examples/gno.land/r/test/katanafixtures/z_renders_filetest.gno
   cp z_frac_filetest.gno.txt    $ROOT/examples/gno.land/r/test/katanafixtures/frac/z_frac_filetest.gno
   # gnomod.toml: module = "gno.land/r/test/katanafixtures[/frac]", gno = "0.9"
   ```

3. Truncate everything after `// Output:` in each filetest. Run
   `GNOROOT=$ROOT /tmp/gno test -update-golden-tests .` in each package.
   Split the `// Output:` block on the `=== <name> (<id>)` lines into
   `../render/<name>.md`. Strip the `// ` prefix and the trailing newline
   that `println` adds.
