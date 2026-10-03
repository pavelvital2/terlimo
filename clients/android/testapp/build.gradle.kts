import java.io.File

plugins { id("com.android.application") }

// All three trusted deployment inputs must be supplied by the release publisher.
// Empty defaults disable only updating; no TEST origin is silently a production source.
fun updateLiteral(name: String): String {
    val value = providers.gradleProperty(name).orNull.orEmpty()
    require(value.none { it.code < 32 }) { "Invalid update deployment property" }
    return "\"" + value.replace("\\", "\\\\").replace("\"", "\\\"") + "\""
}

android {
    buildFeatures { buildConfig = true }
    namespace = "xyz.terlimo.test"
    compileSdk = 36
    ndkVersion = "28.2.13676358"
    defaultConfig {
        applicationId = "xyz.terlimo.test"
        minSdk = 28
        targetSdk = 35
        buildConfigField("String", "UPDATE_MANIFEST_URL", updateLiteral("terlimoUpdateManifestUrl"))
        buildConfigField("String", "UPDATE_CHANNEL", updateLiteral("terlimoUpdateChannel"))
        buildConfigField("String", "UPDATE_PATH_PREFIX", updateLiteral("terlimoUpdatePathPrefix"))
        versionCode = 14
        versionName = "0.14-routing"
        ndk { abiFilters += "arm64-v8a" }
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    packaging { jniLibs { useLegacyPackaging = true } }
    buildTypes {
        release { isMinifyEnabled = false }
        create("d1") {
            initWith(getByName("debug"))
            applicationIdSuffix = ".d1"
            versionNameSuffix = "-d1"
            isDebuggable = true
            signingConfig = signingConfigs.getByName("debug")
        }
    }
}

// Fail-fast guard: a testapp APK must never be considered buildable without the
// prebuilt native input. Gradle does not build it; the canonical native-first
// path is ./build-test-android.sh (go build -> jniLibs -> Gradle).
// The path is carried as a serializable task input so the action holds no
// Gradle script object references (configuration-cache safe).
val verifyNativeInput = tasks.register("verifyNativeInput") {
    group = "verification"
    description = "Fail fast unless src/main/jniLibs/arm64-v8a/libterlimo.so is a present, executable ELF."
    inputs.property(
        "nativeInputPath",
        layout.projectDirectory.file("src/main/jniLibs/arm64-v8a/libterlimo.so").asFile.absolutePath
    )
    doLast {
        val nativeInputFile = File(inputs.properties.getValue("nativeInputPath") as String)
        val canonical = "testapp/src/main/jniLibs/arm64-v8a/libterlimo.so"
        val buildCommand = "./build-test-android.sh"
        if (!nativeInputFile.isFile) {
            throw GradleException(
                "Native input missing or not a regular file: $canonical. " +
                    "Refusing to build a testapp APK without it; build the native-first input with $buildCommand."
            )
        }
        if (!nativeInputFile.canExecute()) {
            throw GradleException(
                "Native input is not executable: $canonical. Re-run $buildCommand to rebuild it."
            )
        }
        if (nativeInputFile.length() == 0L) {
            throw GradleException(
                "Native input is empty: $canonical. Re-run $buildCommand to rebuild it."
            )
        }
        val header = ByteArray(4)
        val read = nativeInputFile.inputStream().use { it.read(header) }
        val elfMagic = byteArrayOf(0x7f.toByte(), 0x45.toByte(), 0x4c.toByte(), 0x46.toByte())
        if (read != 4 || !header.contentEquals(elfMagic)) {
            throw GradleException(
                "Native input is not an ELF file: $canonical. Re-run $buildCommand to rebuild it."
            )
        }
    }
}
tasks.matching { it.name == "preBuild" }.configureEach { dependsOn(verifyNativeInput) }

dependencies {
    implementation("androidx.core:core-ktx:1.15.0")
    // Official v20 CAPTCHA managers are coroutine-based (CompletableDeferred/Mutex/withTimeout).
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.7.3")
    implementation("com.wireguard.android:tunnel:1.0.20260102")
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
    implementation("com.google.zxing:core:3.5.4")
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20260522")
    androidTestImplementation("androidx.test:runner:1.6.2")
    androidTestImplementation("androidx.test.ext:junit:1.2.1")
}
