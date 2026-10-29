# AUR packaging: `wpsdav-bin`

This directory contains the AUR package definition for the prebuilt Linux
x86_64 binary. The AUR repository itself must contain only `PKGBUILD` and
`.SRCINFO` — never the binary tarball.

## Release checklist

1. **Publish a release** by pushing a tag (`git tag v0.1.0 && git push origin v0.1.0`).
   The GitHub Actions workflow builds `wpsdav_<version>_linux_amd64.tar.gz`
   and `checksums-sha256.txt`.
2. **Update `pkgver`** in `PKGBUILD` to the new version (without the leading `v`).
3. **Replace the checksum**: copy the SHA-256 for
   `wpsdav_<version>_linux_amd64.tar.gz` from the release's
   `checksums-sha256.txt` into `sha256sums`.
4. **Regenerate `.SRCINFO`** (on Arch, from this directory):
   ```bash
   makepkg --printsrcinfo > .SRCINFO
   ```
5. **Validate locally**:
   ```bash
   makepkg -f
   namcap PKGBUILD
   namcap wpsdav-bin-*.pkg.tar.zst
   sudo pacman -U wpsdav-bin-*.pkg.tar.zst   # optional smoke test
   ```

## First-time submission to the AUR

```bash
# 1. One-time SSH key setup
ssh-keygen -f ~/.ssh/aur
# ~/.ssh/config:
#   Host aur.archlinux.org
#     IdentityFile ~/.ssh/aur
#     User aur
# Then add ~/.ssh/aur.pub to https://aur.archlinux.org/ (My Account → SSH Public Key)

# 2. Clone the (empty) AUR repository
git -c init.defaultBranch=master clone ssh://aur@aur.archlinux.org/wpsdav-bin.git

# 3. Copy the package definition and push
cp PKGBUILD .SRCINFO wpsdav-bin/
cd wpsdav-bin
git add PKGBUILD .SRCINFO
git commit -m "wpsdav-bin 0.1.0"
git push
```

Subsequent updates reuse the same clone: edit `PKGBUILD`, regenerate
`.SRCINFO`, commit and push to the `master` branch.

## Notes

- `options=('!strip')` prevents makepkg from touching the already-stripped
  release binary.
- The release tarball ships `LICENSE`, which is installed to
  `/usr/share/licenses/wpsdav-bin/`.
- Users can install with any AUR helper, e.g. `yay -S wpsdav-bin`.
- Release archives from 0.1.1 on ship `wpsdav.service`; the PKGBUILD installs it
  to `/usr/lib/systemd/user/` when present.
