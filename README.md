# plz

plz is a companion CLI tool for managing reviews on [plz.review](plz.review).

## Development quick start

```
# Set up your development environment
make install
source .build/bin/activate

# Build and test the CLI
make
plz --version
```

## Releasing a new version

1. Bump the version number in VERSION

2. Commit and push VERSION and any other changes

3. Run `make release`
