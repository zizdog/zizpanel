// uploadlimit.js —— 上传大小上限相关的**纯文案函数**（不碰 DOM、不发请求）。
//
// 历史：这里原来还有 planUploadBatches（"每次请求 ≤ 上限"的分批纯函数）与
// uploadReserve。2026-09-26 上传面板重做后，生产路径改成**每个文件一个请求**
// （见 files.js 的 sendUploadRequest 与 uploadqueue.js）——只有这样才能给出真正的
// 逐文件进度，所以分批逻辑（含它的门禁 upload_batch_gate_test.go）已彻底删除，
// 不再保留"参考实现"（避免以后有人误以为线上还在分批）。
//
// 现在只剩"单个文件太大"时给用户的出路，仍被 files.js 与上传队列的失败行使用。

// oversizeAdvice 是"单个文件太大"时给用户的出路。
//
// ⚠️ 绝不许再提"再用一次上传功能"（例如"用上传文件夹分批"）：用户刚用的就是它，
// 那种建议自相矛盾且必然失败 —— 正是这次报障的形态。
export function oversizeAdvice() {
  return [
    '在本机分卷压缩后分次上传',
    '用命令行（终端）直接放进站点目录',
  ];
}
