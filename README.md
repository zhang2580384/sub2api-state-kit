# Sub2API STATE Kit 插件

本仓库当前只维护 Sub2API `v0.2.7` 的独立 `.s2plugin` 插件，不覆盖宿主源码。插件为 OpenAI OAuth 账号提供账号级 Pro / Team STATE、固定出口复验、自动续期、异常重采和实时诊断。

## 核心要求

- 使用第一层固定代理 + B2Proxy API 代理生成器，两层都要配置。
- B2Proxy 必须选择 `GLOBAL`、`HTTP/HTTPS`、`黏性IP`、`180` 分钟。
- “提前更换 IP”必须选择 `否`，API 参数必须包含 `sessTime=180&sessAuto=0`。
- 默认阻止香港 `HK`；未知地区出口同样不使用。
- 建议开启“优先复用账号上一轮可用出口 IP”。
- `sessTime=180` 是供应商会话上限，不代表单张 292 一定能持续 180 分钟；插件会在 312、模型不符或出口失效时自动重采。

本轮实测中，同一粘性出口连续保持 `87.91` 分钟未变；6 张 `292 + gpt-6-astra` 分别持续 `6.05、5.15、5.00、4.11、1.13、1.13` 分钟，之后切回 `312 + gpt-5.6-luna`。`10` 分钟可能成功，但不能作为稳定下限。

## 快速开始

1. 通过 [B2Proxy 注册入口](https://www.b2proxy.com/signup?code=FF10AB) 登录后台。
2. 创建提取链接：`GLOBAL`、数量 `1`、`HTTP/HTTPS`、`TXT`、黏性 IP `180` 分钟、提前更换 IP `否`。
3. 复制完整 `.../gen?...` API 地址，确认其中包含 `sessTime=180&sessAuto=0`。
4. 在 Sub2API 的代理管理中添加一个可用的第一层固定 HTTP(S) 或 SOCKS5(h) 代理。
5. 按 [插件安装与使用指南](docs/plugin.md) 合并发布者公钥、重启宿主并上传同架构 `.s2plugin`。
6. 插件选择“代理生成器”，粘贴 B2Proxy API，保留阻止地区 `HK`，票据和生成器出口有效期都设为 `180` 分钟。

代理生成器模式的实际链路：

```text
插件 -> 第一层代理 -> B2Proxy 生成器 API
插件 -> 第一层代理 -> 生成的 180 分钟粘性代理 -> ChatGPT
```

## AMD64 安装包

Linux AMD64 宿主请下载：

```text
sub2api-state-kit_plugin_v4.1.0_linux-amd64.s2plugin
```

首次安装需要先合并 Release 中的 `trusted-publisher.yaml` 到宿主现有 `config.yaml`，并重启一次 Sub2API 应用服务。已经安装旧版插件时，直接在插件管理上传 AMD64 包升级，配置会保留。

## 文档

- [插件安装、B2Proxy API 和完整配置](docs/plugin.md)
- [自动化和真实链路验证记录](docs/plugin-validation.md)
- [最新 Release](https://github.com/zhang2580384/sub2api-state-kit/releases/latest)

插件不包含账号、代理凭据、API Key、STATE、数据库或签名私钥。上游 Overload、429 和模型策略变化仍可能影响可用性。
