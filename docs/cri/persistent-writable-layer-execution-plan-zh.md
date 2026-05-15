# 持久化 Writable Layer 执行方案（Pod 模式）

## 1. 前提与结论

当前环境中，所有容器都通过 Kubernetes Pod 创建，不存在绕过 Pod 直接调用 containerd CRI 创建业务容器的场景。

因此，本方案的控制入口统一建立在 **Pod / Deployment 语义之上**，而不是容器实例本身。

目标语义统一为：

- **删除 Pod**：视为重启，保留 writable layer
- **删除 Deployment**：视为删除应用，删除对应 writable layer
- **所有持久层均为节点本地持久化**
- **同一持久层仅允许单节点单 writer**

## 2. 总体架构

整体方案由四部分组成：

```text
Kubernetes API Server
  -> Mutating Admission Webhook
  -> Deployment Delete Controller
  -> Node Agent (DaemonSet)
  -> Patched containerd CRI
```

职责分工如下：

### 2.1 Webhook

负责在 Deployment 创建或更新时自动注入持久层相关 annotation。

它解决的问题是：

- 哪些 workload 需要持久 writable layer
- persist id 从哪里来
- workload 元信息如何稳定传递给 containerd

### 2.2 Patched containerd CRI

负责：

- 读取 Pod annotation
- 创建或复用固定 key 的 writable snapshot
- 删除容器时保留 persistent snapshot
- 给 snapshot 打上业务 labels

它解决的问题是：

- Pod 删除后 rootfs 修改不丢
- Pod 重建时继续复用旧 writable layer

### 2.3 Deployment Delete Controller

负责：

- 监听 Deployment 生命周期
- 给目标 Deployment 添加 finalizer
- 在 Deployment 删除时判断是否需要回收 snapshot
- 调用 node-agent 删除节点本地 snapshot

它解决的问题是：

- 什么时候该删 snapshot
- 如何避免 Pod 重启场景误删

### 2.4 Node Agent

负责：

- 跑在每个节点上
- 接收 controller 发来的删除请求
- 在节点本地调用 containerd API 删除 snapshot

它解决的问题是：

- snapshot 是 containerd 节点本地对象，Kubernetes API 不能直接删除

## 3. 数据流与事件流

### 3.1 创建流程

```text
Deployment Create/Update
  -> Webhook 注入 persist annotations
  -> Deployment 生成 Pod
  -> containerd CRI 读取 Pod annotations
  -> 根据 persist id 创建或复用 persistent snapshot
  -> Pod Running
```

### 3.2 Pod 删除流程

```text
Pod Delete
  -> kubelet / CRI 删除 container
  -> containerd CRI 删除容器 metadata，但保留 persistent snapshot
  -> 新 Pod 重新创建时复用原 snapshot
```

### 3.3 Deployment 删除流程

```text
Deployment Delete
  -> Controller 通过 finalizer 拦截删除
  -> Controller 确认没有 Pod 仍在引用 persist id
  -> Controller 调用目标节点 node-agent
  -> Node-agent 删除 persistent snapshot
  -> Controller 移除 finalizer
  -> Deployment 完成删除
```

## 4. 注入策略

由于当前所有容器都通过 Pod 创建，建议统一以 **Deployment 模板** 为入口，向 `spec.template.metadata.annotations` 注入持久层元信息。

推荐注入字段：

```yaml
persist.containerd.dev/enabled: "true"
persist.containerd.dev/id: "<stable-persist-id>"
persist.containerd.dev/workload-kind: "Deployment"
persist.containerd.dev/workload-name: "<deployment-name>"
persist.containerd.dev/workload-namespace: "<namespace>"
persist.containerd.dev/reclaim-policy: "Delete"
```

说明：

- `enabled`
  - 是否启用持久 writable layer
- `id`
  - 逻辑上的持久层标识
- `workload-kind`
  - 当前固定为 `Deployment`
- `workload-name`
  - Deployment 名称
- `workload-namespace`
  - Deployment 所在 namespace
- `reclaim-policy`
  - 建议第一版支持 `Delete` 和 `Retain`

## 5. persist id 生成建议

persist id 必须稳定，且在同一逻辑资源范围内唯一。

建议优先级如下：

### 方案 A：平台显式下发

由平台 API 或上游系统明确给出：

```text
user-a-devbox
```

适合：

- 用户环境
- 开发工作空间
- 有业务主键的场景

### 方案 B：基于 namespace + deployment name 生成

例如：

```text
default-persist-test
```

适合：

- 没有现成业务 ID
- 第一版快速落地

不建议使用随机值，否则 Pod 重建时无法复用同一个 persistent snapshot。

## 6. snapshot key 规则

当前 patched containerd CRI 中的 persistent snapshot key 规则为：

```text
persist/<persist-id>-<container-name>
```

例如：

```text
persist/default-persist-test-box
```

这样做的原因是：

- 一个 Deployment 可能有多个 Pod
- 一个 Pod 可能有多个容器
- 需要避免多个容器共享同一个 writable rootfs

如果 node-agent 从外部 artifact 导入 rootfs 内容，导入结果不能 commit 到最终运行 key。

导入和运行需要拆成两层：

```text
persist-import/<persist-id>-<container-name>   # committed，导入基底
persist/<persist-id>-<container-name>          # active，运行时 writable rootfs
```

原因是容器运行需要 writable rootfs，对应 containerd active snapshot；而导入产物通常是 committed snapshot，只适合作为 parent。

因此 node-agent import 应写入：

```text
persist-import/<persist-id>-<container-name>
```

containerd CRI 创建容器时负责执行：

```text
Prepare("persist/<id>-<container>", "persist-import/<id>-<container>")
```

这样 Pod 启动后能看到导入基底中的文件，同时后续写入仍落在 `persist/...` active snapshot 中。

## 7. Webhook 执行方案

## 7.1 拦截对象

建议拦截：

- `apps/v1`
- `deployments`
- `CREATE`
- `UPDATE`

## 7.2 判断条件

第一版建议只对带特定标记的 Deployment 注入 persist annotation，例如：

```yaml
metadata:
  labels:
    persist.containerd.dev/enabled: "true"
```

或由业务上游调用方在 Deployment 上显式设置一个开关字段，再由 webhook 转换为 Pod template annotations。

## 7.3 Webhook 行为

当 Deployment 满足持久化条件时：

1. 确保 `spec.template.metadata.annotations` 存在
2. 写入 persist annotations
3. 不覆盖用户已显式传入且合法的 persist id
4. 若用户未传 persist id，则自动生成稳定 id

## 7.4 Webhook 输出要求

- 必须幂等
- 重复调用不应产生漂移
- 不应无条件覆盖已有 annotation

## 8. Controller 执行方案

## 8.1 监听对象

建议监听：

- `Deployment`

可选补充监听：

- `Pod`

第一版只监听 Deployment 即可，Pod 仅用于辅助检查“是否仍有引用”。

## 8.2 Finalizer

建议在目标 Deployment 上添加：

```text
persist.containerd.dev/snapshot-protection
```

用途：

- Deployment 删除时先进入 terminating
- 等 snapshot 删除完成后再真正完成对象删除

## 8.3 Reconcile 主流程

伪代码如下：

```text
Reconcile(Deployment):
  1. Get Deployment
  2. 如果不是 persist workload，返回
  3. 如果未删除：
       确保 finalizer 存在
       返回
  4. 如果正在删除：
       读取 persist id / reclaim policy
       如果 reclaim-policy != Delete：
         直接移除 finalizer
         返回
       检查是否还有 Pod 在引用 persist id
       如果有：
         重试
       计算 snapshot keys
       调用 node-agent 删除
       删除成功后移除 finalizer
```

## 8.4 “仍有引用”判断

第一版判断规则建议简单明确：

- 同 namespace 下
- Pod annotation 中 `persist.containerd.dev/id` 相同
- Pod 未处于删除完成状态

如果存在这样的 Pod，则认为 snapshot 仍在使用中，不应删除。

## 9. Node-agent 执行方案

## 9.1 部署方式

建议使用 DaemonSet，每个节点一个实例。

## 9.2 职责

仅做节点本地 containerd 操作，不参与业务语义判断。

职责包括：

- 根据 snapshot key 查询 snapshot 是否存在
- 检查 snapshot 是否仍被运行中容器引用
- 调用 containerd API 删除 snapshot

## 9.3 推荐接口

Controller 调用 node-agent 时，建议提供一个最小删除接口：

```json
{
  "nodeName": "westus",
  "namespace": "default",
  "workloadKind": "Deployment",
  "workloadName": "persist-test",
  "persistID": "default-persist-test",
  "snapshotKeys": [
    "persist/default-persist-test-box"
  ]
}
```

## 9.4 删除动作

node-agent 的真正删除动作应调用 containerd snapshot service 的 `Remove()`。

等价调试命令为：

```sh
sudo ctr --address /run/containerd/containerd.sock --namespace k8s.io snapshots rm persist/default-persist-test-box
```

注意：

- 不能直接删除宿主机 snapshot 目录
- 删除前必须确认无运行中容器占用

## 10. containerd 侧需要保留的实现

当前 patched containerd CRI 已经具备以下能力，应保持不变：

1. 读取 Pod annotation
2. 对启用持久化的容器使用固定 snapshot key
3. 运行 key 已存在时复用 active/view snapshot
4. 运行 key 不存在但 `persist-import/...` committed 基底存在时，以该基底为 parent 创建 active snapshot
5. 两者都不存在时，基于镜像 rootfs 创建新的 active snapshot
6. 删除容器时跳过 persistent snapshot cleanup
7. 持久 snapshot 带业务 labels
8. 校验 image digest 一致性
9. 防止同 snapshot key 被并发双写

这意味着 K8s 侧新增组件不需要再修改 containerd 的核心持久化逻辑，只需要补齐“策略注入”和“生命周期回收”。

## 11. 分阶段落地建议

### 阶段 1：验证阶段

目标：

- 保证持久 writable layer 功能可用

动作：

- 手工在 Deployment 模板中写 persist annotations
- 手工在节点上删除 snapshot
- 不引入 webhook / controller / node-agent

### 阶段 2：半自动阶段

目标：

- Deployment 删除时自动删除 snapshot

动作：

- 引入 Deployment Delete Controller
- 引入 node-agent
- 继续由平台或人工写 annotations

### 阶段 3：正式阶段

目标：

- 完整自动化

动作：

- 引入 Mutating Admission Webhook
- Deployment 创建时自动注入 annotations
- Controller + node-agent 自动回收 snapshot

## 12. 风险与约束

### 12.1 节点本地性

当前方案是 **node-local persistent rootfs**：

- Pod 调度到原节点时可复用
- 调度到其他节点时默认无法复用

如需跨节点迁移，需要额外实现 snapshot 导出/导入。

### 12.2 单 writer 约束

同一 persist id 不应允许两个运行中容器同时使用同一 writable snapshot。

当前 CRI 已加本地防护，但控制面也应避免创建这种冲突拓扑。

### 12.3 删除判断必须幂等

Controller 与 node-agent 都必须设计为可重试：

- snapshot 已不存在时视为成功
- 删除过程中节点暂时不可达时可重试

### 12.4 不应直接依赖 snapshot 物理目录

逻辑上应依赖 snapshot key 与 containerd API。

不应直接操作宿主机上的 snapshot 目录结构。

## 13. 验收标准

### 13.1 Pod 重建保留数据

验证步骤：

1. 创建带 persist annotation 的 Deployment
2. 写入 `/root/persist-marker`
3. 删除 Pod
4. 等待 Deployment 重建 Pod
5. 新 Pod 中仍可读到原文件内容

### 13.2 Deployment 删除时自动回收

验证步骤：

1. Deployment 运行时确认 snapshot 存在
2. 删除 Deployment
3. Controller 触发 node-agent
4. 节点上对应 snapshot 被删除
5. Deployment finalizer 清理完成

### 13.3 重复删除安全

验证：

- snapshot 已不存在时再次触发删除，不应报致命错误

## 14. 最终建议

在“所有容器都经由 Pod 创建”的前提下，最推荐的执行路线是：

1. **保留当前 patched containerd CRI**
2. **通过 Webhook 在 Deployment 入口自动注入 persist annotations**
3. **通过 Deployment Delete Controller + finalizer 控制回收时机**
4. **通过 node-agent 在节点本地执行 snapshot 删除**

这样可以在不修改 Kubernetes 核心源码的前提下，把“Pod 删除保留、Deployment 删除回收”的语义完整落地。
