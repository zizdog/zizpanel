// netfail.js —— 网络失败判据与统一文案的**唯一前端真源**。
//
// 判据与 internal/services/netfail.go 同源，NET_HINT_MARKER 必须与
// services.NetworkHintMarker 逐字一致；各页面只 import，不再各抄一份证据表。
import { h } from './ui.js';

export const NET_HINT_MARKER = '面板不会替你翻墙';
export const NET_HINT_ONE_LINE = '🌐 网络问题：面板不会替你翻墙，请自备代理/梯子后重试';

const NET_EVIDENCE = [
  'could not resolve host', 'temporary failure in name resolution',
  'name or service not known', 'no such host', 'server misbehaving',
  'connection refused', 'connection timed out', 'connection timeout',
  'connection reset by peer', 'no route to host', 'network is unreachable',
  'network is down', 'host is down', 'i/o timeout', 'operation timed out',
  'failed to connect to', "couldn't connect to server", 'could not connect to server',
  'dial tcp', 'dial udp',
  'tls handshake timeout', 'tls: failed to verify certificate',
  'tls: bad certificate', 'remote error: tls:', 'x509:',
  'certificate signed by unknown authority', 'certificate is not valid for',
  'client.timeout exceeded', 'request canceled while waiting for connection',
  // curl 特定退出码（刻意排除 22：HTTP 错误码，是"镜像上没有文件"不是网络不通）
  'curl: (6)', 'curl: (7)', 'curl: (28)', 'curl: (35)', 'curl: (52)',
  'curl: (56)', 'curl: (60)', 'ssl connect error',
];
const NET_LOCAL_ENDPOINT = ['unix://', 'dial unix', 'docker.sock', '127.0.0.1', 'localhost', '[::1]'];
const NET_LOCAL_OVERRIDE = ['proxyconnect'];

// 只认有真实证据的网络失败，拿不准 false：误报比漏报更糟。
export function isNetworkFailureText(text) {
  const msg = String(text == null ? '' : text).toLowerCase();
  if (!msg.trim()) return false;
  if (msg.includes(NET_HINT_MARKER)) return true;
  if (NET_LOCAL_OVERRIDE.some((s) => msg.includes(s))) return true; // 走代理失败优先
  if (NET_LOCAL_ENDPOINT.some((s) => msg.includes(s))) return false; // 本地服务没起来 ≠ 要翻墙
  if (NET_EVIDENCE.some((s) => msg.includes(s))) return true;
  if (msg.includes('context deadline exceeded')
    && (msg.includes('http://') || msg.includes('https://'))) return true;
  return false;
}

// networkHintText 是可直接 toast 的一句话（≤40 字）。
export function networkHintText() {
  return NET_HINT_ONE_LINE;
}

// networkHintEntries 渲染"改镜像/代理的入口"折叠项；entriesText 由各页给
// （各页入口不同：应用市场 / 一键建站 / Docker / 升级），细节收起来。
export function networkHintEntries(entriesText) {
  return h('details', { style: { marginTop: '6px' } }, [
    h('summary', { style: { cursor: 'pointer', color: 'var(--text-dim)' }, text: '面板里改镜像/代理的入口' }),
    h('div', { style: { marginTop: '4px', color: 'var(--text-dim)' }, text: entriesText }),
  ]);
}

// networkHintBlock 是醒目展示块：danger 底 + pill + 一句话 + 折叠入口 + 原文。
export function networkHintBlock(errText, entriesText) {
  const raw = String(errText == null ? '' : errText).trim();
  const nodes = [
    h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      h('span.pill.danger', { style: { fontSize: '12px', fontWeight: '700' }, text: '🌐 网络问题' }),
      h('strong', { text: NET_HINT_ONE_LINE }),
    ]),
    networkHintEntries(entriesText),
  ];
  if (raw) {
    nodes.push(h('div', {
      style: {
        marginTop: '6px', fontFamily: 'var(--mono)', fontSize: '11.5px',
        color: 'var(--text-dim)', wordBreak: 'break-all',
      },
      text: '原始报错：' + raw,
    }));
  }
  return h('div', {
    dataset: { testid: 'zp-network-failure' },
    style: {
      padding: '11px 13px', background: 'var(--danger-soft)',
      border: '1px solid var(--danger)', borderRadius: 'var(--radius)',
      fontSize: '12.5px', lineHeight: '1.7', marginBottom: '12px',
    },
  }, nodes);
}
