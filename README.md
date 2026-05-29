
## Module wrapper versioning

We use a simple versioning scheme where the the upstream controls the major and minor version and `W` gets incremented whenever something in the wrapper changes (e.g. `hugo.toml` config update):

>vX.Y.(Z*1000+W)

`W` starts at 0 and resets on any upstream-change.