# plz

plz is a companion CLI tool for managing reviews on [plz.review](https://plz.review).

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

Make sure that the git repo is clean and up to date with origin/main. Then run:

```
(read -r v && git tag -a v$v -m v$v && git push origin v$v)
```

## Known setup issues

Should you encounter this error:
```
ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain
```

Please run the following commands:
```
chmod 600 ~/.ssh/id_rsa
ssh-add ~/.ssh/id_rsa
```