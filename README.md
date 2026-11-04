# VoiceKey 发布仓库（公开）

VoiceKey Mac 端的分发元数据与源码快照（brew 安装时本地编译）。

- `macrecv/`：Go 源码，brew 安装时下载本仓库 tarball 并在本机编译
- `version.txt`：最新版本号，程序启动时异步检查，发现新版本走 `brew upgrade voicekey`
- 主代码仓库（私有）：chencanxin/voicekey

## 安装

    brew install chencanxin/tap/voicekey

## 发版流程（维护者）

1. 更新 `macrecv/` 源码与 `version.txt`（版本号一致）
2. 提交后取 commit sha，更新 tap 仓库 `Formula/voicekey.rb` 的 `url`（codeload 按 commit sha 固定）与 `sha256`
