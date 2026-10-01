#!/usr/bin/env bash
set -euo pipefail
source /home/pavel/.config/android-build.env
cd /home/pavel/step036-receipts/verified-publication-build-20261001
cp /home/pavel/terlimo-verified-catalog-publication-20261001/testapp/build/outputs/apk/debug/testapp-debug.apk candidate.apk
unzip -p candidate.apk lib/arm64-v8a/libterlimo.so > candidate-packaged-native.so
cp candidate-native.so strip-check.so
ndk_bin="$ANDROID_SDK_ROOT/ndk/28.2.13676358/toolchains/llvm/prebuilt/linux-x86_64/bin"
"$ndk_bin/llvm-strip" --strip-unneeded strip-check.so
cmp strip-check.so candidate-packaged-native.so
"$ndk_bin/llvm-readelf" -h -l candidate-native.so > native-elf.txt
"$ndk_bin/llvm-readelf" -h -l candidate-packaged-native.so > packaged-elf.txt
go version -m candidate-packaged-native.so > packaged-buildinfo.txt
"$ANDROID_SDK_ROOT/build-tools/36.0.0/apksigner" verify --verbose --print-certs candidate.apk > candidate-signature.txt
"$ANDROID_SDK_ROOT/build-tools/36.0.0/aapt2" dump badging candidate.apk > badging.txt
"$ANDROID_SDK_ROOT/build-tools/36.0.0/zipalign" -c -P 16 -v 4 candidate.apk > alignment.txt
git -C /home/pavel/terlimo-verified-catalog-publication-20261001 status --short > source-status.txt
git -C /home/pavel/terlimo-verified-catalog-publication-20261001 diff --check
sha256sum candidate.apk candidate-native.so candidate-packaged-native.so
