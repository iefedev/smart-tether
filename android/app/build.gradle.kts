import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.plugin.compose")
}

// Release signing never lives in the repo. Each value comes from, in order:
//   1. an environment variable or Gradle property (-P or ~/.gradle/gradle.properties):
//      ANDROID_KEYSTORE_FILE, ANDROID_KEYSTORE_PASSWORD, ANDROID_KEY_ALIAS, ANDROID_KEY_PASSWORD
//   2. a keystore.properties file (storeFile, storePassword, keyAlias, keyPassword) at the path in
//      SMART_TETHER_KEYSTORE, or ~/.smart-tether/keystore.properties (tools/new-keystore.ps1).
// A release build without a complete signing config fails; it is never signed with the debug key.
val keystoreProps = Properties().apply {
    val f = file(System.getenv("SMART_TETHER_KEYSTORE")
        ?: "${System.getProperty("user.home")}/.smart-tether/keystore.properties")
    if (f.exists()) f.inputStream().use { load(it) }
}

fun signingValue(name: String, fileKey: String): String? =
    (System.getenv(name) ?: providers.gradleProperty(name).orNull ?: keystoreProps.getProperty(fileKey))
        ?.takeIf { it.isNotBlank() }

val releaseStoreFile = signingValue("ANDROID_KEYSTORE_FILE", "storeFile")
val releaseStorePassword = signingValue("ANDROID_KEYSTORE_PASSWORD", "storePassword")
val releaseKeyAlias = signingValue("ANDROID_KEY_ALIAS", "keyAlias")
val releaseKeyPassword = signingValue("ANDROID_KEY_PASSWORD", "keyPassword")
val releaseSigningReady = listOf(releaseStoreFile, releaseStorePassword, releaseKeyAlias, releaseKeyPassword)
    .all { it != null }

android {
    namespace = "dev.smarttether"
    compileSdk = 37 // core-ktx 1.19 requires it; targetSdk stays 34

    defaultConfig {
        applicationId = "dev.smarttether"
        minSdk = 31
        targetSdk = 34
        versionCode = 3
        versionName = "0.2.0"
    }

    signingConfigs {
        if (releaseSigningReady) {
            create("release") {
                storeFile = file(releaseStoreFile!!)
                storePassword = releaseStorePassword
                keyAlias = releaseKeyAlias
                keyPassword = releaseKeyPassword
            }
        }
    }

    buildFeatures {
        compose = true
    }

    packaging {
        jniLibs.useLegacyPackaging = false
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // No fallback: without the release key the APK stays unsigned and the check below fails the build.
            signingConfig = signingConfigs.findByName("release")
        }
    }
}

gradle.taskGraph.whenReady {
    if (!releaseSigningReady && allTasks.any { it.project == project && it.name.contains("Release") && it.name.startsWith("package") }) {
        throw GradleException(
            "Release signing is not configured. Set ANDROID_KEYSTORE_FILE, ANDROID_KEYSTORE_PASSWORD, " +
                "ANDROID_KEY_ALIAS and ANDROID_KEY_PASSWORD, or run tools/new-keystore.ps1.",
        )
    }
}

dependencies {
    // Built by tools/build-android.ps1 (gomobile bind ./mobile).
    implementation(files("libs/core.aar"))

    val composeBom = platform("androidx.compose:compose-bom:2026.09.00")
    implementation(composeBom)
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-extended")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.activity:activity-compose:1.13.0")
    implementation("androidx.core:core-ktx:1.19.1")
    // QR scanning of the PC's pairing code (camera screen included).
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
}
