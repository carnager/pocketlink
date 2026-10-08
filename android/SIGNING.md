# Signing release builds

No keys or passwords live in this repository. `app/build.gradle.kts` reads
the signing values at build time from, in order of precedence:

1. environment variables of the same name (handy for CI), then
2. the properties file named by `POCKETLINK_ANDROID_SIGNING_PROPERTIES`, or
   by default `~/.local/android/release-keys/pocketlink.properties`.

| Key                                 | Meaning                         |
|-------------------------------------|---------------------------------|
| `POCKETLINK_ANDROID_STORE_FILE`     | path to the keystore            |
| `POCKETLINK_ANDROID_STORE_PASSWORD` | keystore password               |
| `POCKETLINK_ANDROID_KEY_ALIAS`      | key alias (`pocketlink`)        |
| `POCKETLINK_ANDROID_KEY_PASSWORD`   | key password                    |

`signing.properties.example` is a template. If none of this is set,
`assembleRelease` still works but signs with the local debug key, which is
fine for trying things out and not for publishing.

## The maintainer's setup

Release keys live outside the repo in `~/.local/android/release-keys/`
(mode 700, files 600): one shared PKCS12 `release.keystore` with an alias per
app, and one `<app>.properties` per app pointing at it. The pocketlink key is
alias `pocketlink` (RSA 3072, valid 10000 days); its certificate's SHA-256 is

    9C:BB:73:0D:CD:0A:41:6C:ED:93:DF:7F:57:F9:A6:EA:D3:9D:94:EE:F0:33:19:4E:97:57:7D:27:62:66:0B:B8

which `apksigner verify --print-certs` must show for published APKs.
Back up `release.keystore` before changing it
(`release.keystore.bak-<app>-<date>-<time>`); a lost key means users have to
uninstall and reinstall to get updates.

## Building and publishing

```sh
cd android
./gradlew assembleRelease
apksigner verify --print-certs app/build/outputs/apk/release/app-release.apk
gh release upload vX.Y.Z app/build/outputs/apk/release/app-release.apk#pocketlink-X.Y.Z.apk
```
