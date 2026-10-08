# Components are referenced from the manifest, which R8 already keeps.
# The QR scanner library loads its decoder hints reflectively.
-keep class com.google.zxing.** { *; }
-dontwarn com.google.zxing.**
