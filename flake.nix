{
  description = "Monetdroid development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config = {
            android_sdk.accept_license = true;
            allowUnfree = true;
          };
        };

        buildToolsVersion = "34.0.0";
        androidComposition = pkgs.androidenv.composeAndroidPackages {
          buildToolsVersions = [ buildToolsVersion ];
          platformVersions = [ "34" ];
          includeEmulator = false;
          includeSources = false;
          includeSystemImages = false;
          abiVersions = [ "arm64-v8a" ];
          extraLicenses = [
            "android-googletv-license"
            "android-sdk-arm-dbt-license"
            "android-sdk-preview-license"
            "google-gdk-license"
            "intel-android-extra-license"
            "intel-android-sysimage-license"
            "mips-android-sysimage-license"
          ];
        };

        androidSdk = androidComposition.androidsdk;
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = [
            pkgs.go
            pkgs.jdk17
            androidSdk
            pkgs.gradle
            pkgs.kotlin
          ];

          ANDROID_HOME = "${androidSdk}/libexec/android-sdk";
          ANDROID_SDK_ROOT = "${androidSdk}/libexec/android-sdk";
          JAVA_HOME = pkgs.jdk17.home;

          shellHook = ''
            echo "Monetdroid dev environment"
            echo "  Go:          $(go version | cut -d' ' -f3)"
            echo "  Java:        $(java -version 2>&1 | head -1)"
            echo "  Android SDK: $ANDROID_SDK_ROOT"
            echo ""
            echo "Commands:"
            echo "  go build -o monetdroid .                        # build server"
            echo "  cd android && ./gradlew assembleDebug           # build APK"
            echo "  adb install android/app/build/outputs/apk/debug/app-debug.apk"

            # Create local.properties for Gradle
            if [ -d android ] && [ ! -f android/local.properties ]; then
              echo "sdk.dir=$ANDROID_SDK_ROOT" > android/local.properties
              echo "Created android/local.properties"
            fi
          '';
        };
      });
}
