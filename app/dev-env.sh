export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
export SDKROOT="${SDKROOT:-$(DEVELOPER_DIR=$DEVELOPER_DIR xcrun --sdk macosx --show-sdk-path)}"
