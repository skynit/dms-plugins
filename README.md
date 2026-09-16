# DMS Plugins

为 [DankMaterialShell（DMS）](https://github.com/AvengeMedia/DankMaterialShell) 提供的三个本地插件。每个插件都可以独立安装、启用和放置，界面跟随 DMS 主题。

| 插件 | 功能 | 详细说明 |
| --- | --- | --- |
| **LocalStatus** | IP、网络连接、CPU、系统负载、内存、Swap、磁盘剩余空间和时间；支持模块开关、单双行、列对齐及间距调节 | [安装、设置与开发](LocalStatus/README.md) |
| **LocalTranslate** | 内置 CC-CEDICT 词库，中文与英文双向离线查词；显示简体中文，支持词条和短语 | [安装、使用与词库许可](LocalTranslate/README.md) |
| **LocalLyrics** | 读取本地音乐文件的内嵌歌词，跟随播放进度显示；支持单双行、时间偏移及复原，无音乐时隐藏 | [安装、歌词来源与播放器支持](LocalLyrics/README.md) |

## 预览

### LocalStatus

![系统状态条](LocalStatus/docs/images/status-bar.png)

### LocalTranslate

![离线英中查词](LocalTranslate/docs/images/english-to-chinese.png)

### LocalLyrics

![双行歌词，使用原创测试文本](LocalLyrics/docs/images/bar-double-example.png)

图片由实际 DMS 组件渲染。歌词示例为原创测试文本，具体颜色由 DMS 主题决定。

## 获取源码与安装

```bash
mkdir -p ~/workspace
git clone git@github.com:skynit/dms-plugins.git ~/workspace/dms-plugins
cd ~/workspace/dms-plugins
```

进入所需插件目录，再按照其 README 复制运行文件：

```bash
cd LocalStatus
# 按 LocalStatus/README.md 的安装步骤操作
```

也可以选择 `LocalTranslate` 或 `LocalLyrics`。不要直接将整个仓库复制为一个 DMS 插件；各插件的 `plugin.json` 需要位于各自安装目录的顶层。

默认安装目录与插件 ID：

| 源码目录 | 默认安装目录 | 插件 ID |
| --- | --- | --- |
| `LocalStatus/` | `~/.config/DankMaterialShell/plugins/LocalStatus` | `localStatus` |
| `LocalTranslate/` | `~/.config/DankMaterialShell/plugins/localTranslate` | `localTranslate` |
| `LocalLyrics/` | `~/.config/DankMaterialShell/plugins/localLyrics` | `localLyrics` |

安装步骤支持通过 `XDG_CONFIG_HOME` 指定配置根目录。复制后运行 `dms ipc call plugin-scan scan`，等待发现插件，再在 DMS 设置中启用并自行选择状态栏位置。源码目录与安装目录是独立副本；更新源码后，需要重新复制对应插件的运行文件。

## 依赖与功能边界

- 运行环境为 Linux、DMS 和对应的 Quickshell/Qt 6；各插件的版本要求见各自 README。
- LocalStatus 的系统采样依赖和可选能力详见其 README。
- LocalTranslate 使用 Python 3 标准库和随仓库提供的 SQLite 词典，不需要在线翻译服务。它只查询已收录的词条、短语，不执行任意整句翻译。
- LocalLyrics 使用 Python 3 和 FFmpeg 的 `ffprobe`，通过 MPRIS 获取播放器状态。运行时读取音频内嵌歌词；不搜索在线歌词，也不读取外置 LRC。缺少音频地址的 termusic 可通过本地媒体库匹配文件。

## 开发验证

仅运行所修改插件的测试，在对应目录执行：

```bash
# LocalStatus/
node --test tests/status.test.cjs

# LocalTranslate/ 或 LocalLyrics/
python3 -m unittest discover -s tests -v
```

歌词测试还需要 FFmpeg 和 Node.js。逻辑测试不能替代实际 DMS 中的界面验证；修改 QML 后应检查对应插件的实际底栏、弹窗和设置。DMS 热重载可能缓存引用的 QML，详情见各插件的更新说明。

## 词库署名与许可

LocalTranslate 内置 CC-CEDICT 原始快照和派生 SQLite 索引，均遵循 **CC BY-SA 4.0**。分发时请保留 [词库署名与变更说明](LocalTranslate/data/NOTICE.md) 和 [完整许可](LocalTranslate/data/CC-BY-SA-4.0.txt)。该数据许可不代表插件源码采用相同许可。
