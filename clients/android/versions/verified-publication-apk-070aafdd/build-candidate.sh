#!/usr/bin/env bash
set -euo pipefail
cd -- /home/pavel/terlimo-verified-catalog-publication-20261001
source /home/pavel/.config/android-build.env
source tools/native-input-guard.sh
export GOMAXPROCS=2
unset SERVICE_FRAME_DIAG
export GOPROXY=off GOTOOLCHAIN=local
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
go version -m testapp/src/main/jniLibs/arm64-v8a/libterlimo.so > /home/pavel/step036-receipts/verified-publication-build-20261001/native-buildinfo.txt
cp testapp/src/main/jniLibs/arm64-v8a/libterlimo.so /home/pavel/step036-receipts/verified-publication-build-20261001/candidate-native.so
sha256sum /home/pavel/step036-receipts/verified-publication-build-20261001/candidate-native.so
gradle --offline --no-daemon --max-workers=2 -Pandroid.builder.sdkDownload=false \
  -Dorg.gradle.jvmargs='-Xmx1536m -XX:MaxMetaspaceSize=512m -Dfile.encoding=UTF-8' \
  :testapp:assembleDebug
# Source-only Go delta: existing reviewed tests are not repeated.
guard_require_apk_elf_entry testapp/build/outputs/apk/debug/testapp-debug.apk "lib/arm64-v8a/libterlimo.so" "packaged native library"
"$ANDROID_SDK_ROOT/build-tools/36.0.0/apksigner" verify --verbose testapp/build/outputs/apk/debug/testapp-debug.apk
"$ANDROID_SDK_ROOT/build-tools/36.0.0/zipalign" -c -P 16 -v 4 testapp/build/outputs/apk/debug/testapp-debug.apk
sha256sum testapp/build/outputs/apk/debug/testapp-debug.apk
