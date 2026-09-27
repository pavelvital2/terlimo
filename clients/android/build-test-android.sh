#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
source /home/pavel/.config/android-build.env
source tools/native-input-guard.sh
export GOMAXPROCS=2
ndk_toolchain="$ANDROID_SDK_ROOT/ndk/28.2.13676358/toolchains/llvm/prebuilt/linux-x86_64"
mkdir -p testapp/src/main/jniLibs/arm64-v8a
(
  cd go_client
  CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
  CC="$ndk_toolchain/bin/aarch64-linux-android28-clang" \
  go build -p 2 -trimpath -buildmode=pie \
    -ldflags='-s -w -checklinkname=0 -linkmode=external -extldflags=-Wl,-z,max-page-size=16384' \
    -o ../testapp/src/main/jniLibs/arm64-v8a/libterlimo.so .
)
guard_require_elf_input testapp/src/main/jniLibs/arm64-v8a/libterlimo.so "freshly built native input"
gradle --no-daemon --max-workers=2 -Pandroid.builder.sdkDownload=false \
  -Dorg.gradle.jvmargs='-Xmx1536m -XX:MaxMetaspaceSize=512m -Dfile.encoding=UTF-8' \
  :testapp:assembleDebug
# PackagedNodeProbeTest reads the final APK, so it must run after assembleDebug.
gradle --no-daemon --max-workers=2 -Pandroid.builder.sdkDownload=false \
  -Dorg.gradle.jvmargs='-Xmx1536m -XX:MaxMetaspaceSize=512m -Dfile.encoding=UTF-8' \
  :testapp:testDebugUnitTest
guard_require_apk_elf_entry testapp/build/outputs/apk/debug/testapp-debug.apk "lib/arm64-v8a/libterlimo.so" "packaged native library"
"$ANDROID_SDK_ROOT/build-tools/36.0.0/apksigner" verify --verbose testapp/build/outputs/apk/debug/testapp-debug.apk
"$ANDROID_SDK_ROOT/build-tools/36.0.0/zipalign" -c -P 16 -v 4 testapp/build/outputs/apk/debug/testapp-debug.apk
sha256sum testapp/build/outputs/apk/debug/testapp-debug.apk
