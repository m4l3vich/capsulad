use std::env;

fn main() {
    let target_os = env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    match target_os.as_str() {
        "linux" => {
            println!("cargo:rustc-cdylib-link-arg=-Wl,-soname,libcapsulad.so.1");
        }
        "macos" => {
            println!("cargo:rustc-cdylib-link-arg=-Wl,-install_name,@rpath/libcapsulad.1.dylib");
            println!("cargo:rustc-cdylib-link-arg=-Wl,-compatibility_version,1.0.0");
            println!("cargo:rustc-cdylib-link-arg=-Wl,-current_version,1.0.0");
        }
        _ => {}
    }

    // The embedded descriptor set covers every framework service. It is
    // regenerated from the canonical proto tree; see sdk/native/Makefile.
    println!("cargo:rerun-if-changed=descriptors/capsulad-framework.protoset");
}
