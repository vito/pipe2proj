# pipe2proj

A tool for converting a pipeline into a project. Extracts tasks, resources, and
resource types into separate config files and pretty-prints them along the way.

Can be run multiple times against the same project. It will error if there are
any conflicts for any of the extracted tasks/resources/etc.

## building

This project uses a few templates under `tmpl/` for rendering pretty-printed
config files. Use `packr build` to bake them into the binary:

```sh
$ go install github.com/gobuffalo/packr/v2/packr2
$ packr2 install
```

After this you should be able to run `pipe2proj` from any directory (assuming
your `$GOPATH/bin` is on your `$PATH`).
