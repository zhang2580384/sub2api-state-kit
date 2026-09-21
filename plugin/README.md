# STATE Kit 独立插件

Sub2API v0.2.7 的开源 OpenAI OAuth transport 插件，提供逐账号 Pro / Team STATE 配置、账号级出口选择、固定业务代理复验、续期和响应异常守护。

请先阅读 [安装与使用说明](../docs/plugin.md)。配置在「插件管理 → STATE Kit → 配置」，不用修改宿主源码。首次安装需在宿主信任本插件的发布者公钥。

- 插件 ID：`io.github.wangyunjeff.sub2api-state-kit`
- 插件版本：`0.3.9`
- 账号显示：账号选择框显示 ID、名称、邮箱；账号资料保存名称、邮箱、到期时间和额度
- 账号出口：可选 Sub2 原有代理、固定 provider session 粘性代理，或由插件调用代理生成器 API 取得固定出口
- 生成器过滤：默认阻止香港 `HK`，未知出口地区同样拒绝；生成器出口按较短 TTL 采集、复核并随票据恢复
- 模型输入：可选用常用模型建议，也可直接输入自定义模型
- 实时诊断监听：打开面板才开始，关闭即停止并清空；新事件置顶，不保存历史
- 协议：Sub2API 插件协议 / transport / UI Bridge / HostService v1
- 宿主源码基线：官方 `v0.2.7`，提交 `aea725f2ea644d5592d0bbb1d63b607efa7e200a`
- 当前部署范围：单应用实例
- 默认：所有 STATE 开关关闭，不含任何真实账号或代理配置

## 开发

```bash
go test -race ./...
node --test ui-tests/*.test.cjs
go build -trimpath -o build/state-kit ./cmd/state-kit
```

`internal/pluginapi/v1` 来自上述官方源码的公开契约，保留源码和许可归属。其余实现为本项目独立实现，不包含官方闭源传输插件。许可证沿用本仓库 LGPL-3.0。

发布者签名公钥在 `release/`；签名私钥必须在仓库外。参见 `../scripts/package_plugin.py` 和 `integration/` 中的宿主安装/运行集成测试。
