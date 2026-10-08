plugins {
    id("com.android.application")
}

android {
    namespace = "dev.tether"
    compileSdk = 36
    buildToolsVersion = "36.1.0"

    defaultConfig {
        applicationId = "dev.tether"
        minSdk = 29
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"))
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

dependencies {
    implementation("com.squareup.okhttp3:okhttp:5.3.2")
    implementation("androidx.activity:activity:1.13.0")
    // QR scanning without Google Play Services.
    implementation("com.journeyapps:zxing-android-embedded:4.3.0")
}
