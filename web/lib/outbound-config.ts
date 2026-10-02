// The "basics" tab of the outbound editor: a form model and its conversion to and from the xray
// outbound JSON the backend stores. The shapes mirror what the backend's link parser produces, so
// a config imported from a share link opens in the form unchanged.

export type Protocol = "vless" | "vmess" | "trojan" | "shadowsocks" | "socks" | "http"
export type Transport = "tcp" | "ws" | "grpc" | "httpupgrade" | "xhttp" | "kcp"
export type Security = "none" | "tls" | "reality"

export interface BasicForm {
  protocol: Protocol
  address: string
  port: number
  // credentials (which ones apply depends on the protocol)
  id: string
  flow: string
  vmessSecurity: string
  password: string
  method: string
  user: string
  pass: string
  // transport
  transport: Transport
  path: string
  host: string
  serviceName: string
  mode: string
  // security
  security: Security
  sni: string
  fingerprint: string
  alpn: string
  allowInsecure: boolean
  publicKey: string
  shortId: string
  spiderX: string
}

export const protocols: { value: Protocol; label: string }[] = [
  { value: "vless", label: "VLESS" },
  { value: "vmess", label: "VMess" },
  { value: "trojan", label: "Trojan" },
  { value: "shadowsocks", label: "Shadowsocks" },
  { value: "socks", label: "SOCKS" },
  { value: "http", label: "HTTP" },
]

export const transports: { value: Transport; label: string }[] = [
  { value: "tcp", label: "TCP" },
  { value: "ws", label: "WebSocket" },
  { value: "grpc", label: "gRPC" },
  { value: "httpupgrade", label: "HTTPUpgrade" },
  { value: "xhttp", label: "XHTTP" },
  { value: "kcp", label: "mKCP" },
]

export const fingerprints = ["", "chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq", "random", "randomized"]

export const ssMethods = [
  "aes-256-gcm",
  "aes-128-gcm",
  "chacha20-ietf-poly1305",
  "xchacha20-ietf-poly1305",
  "2022-blake3-aes-128-gcm",
  "2022-blake3-aes-256-gcm",
  "2022-blake3-chacha20-poly1305",
]

export const vmessCiphers = ["auto", "aes-128-gcm", "chacha20-poly1305", "none", "zero"]

/** Protocols that carry a stream (transport + TLS) layer in the form. */
export const hasStream = (p: Protocol) => p === "vless" || p === "vmess" || p === "trojan"

export function emptyForm(protocol: Protocol = "vless"): BasicForm {
  return {
    protocol,
    address: "",
    port: 443,
    id: "",
    flow: "",
    vmessSecurity: "auto",
    password: "",
    method: "aes-256-gcm",
    user: "",
    pass: "",
    transport: "tcp",
    path: "/",
    host: "",
    serviceName: "",
    mode: "",
    security: protocol === "trojan" ? "tls" : "none",
    sni: "",
    fingerprint: "chrome",
    alpn: "",
    allowInsecure: false,
    publicKey: "",
    shortId: "",
    spiderX: "",
  }
}

type Obj = Record<string, unknown>

function stream(f: BasicForm): Obj | undefined {
  if (!hasStream(f.protocol)) return undefined
  const ss: Obj = { network: f.transport }
  switch (f.transport) {
    case "ws": {
      const ws: Obj = { path: f.path || "/" }
      if (f.host) ws.headers = { Host: f.host }
      ss.wsSettings = ws
      break
    }
    case "grpc":
      ss.grpcSettings = { serviceName: f.serviceName, multiMode: f.mode === "multi" }
      break
    case "httpupgrade": {
      const h: Obj = { path: f.path || "/" }
      if (f.host) h.host = f.host
      ss.httpupgradeSettings = h
      break
    }
    case "xhttp": {
      const x: Obj = { path: f.path || "/" }
      if (f.host) x.host = f.host
      if (f.mode) x.mode = f.mode
      ss.xhttpSettings = x
      break
    }
    case "kcp":
      ss.kcpSettings = { header: { type: "none" } }
      break
  }
  ss.security = f.security
  if (f.security === "tls") {
    const tls: Obj = {}
    if (f.sni) tls.serverName = f.sni
    if (f.fingerprint) tls.fingerprint = f.fingerprint
    const alpn = f.alpn.split(",").map((s) => s.trim()).filter(Boolean)
    if (alpn.length) tls.alpn = alpn
    if (f.allowInsecure) tls.allowInsecure = true
    ss.tlsSettings = tls
  } else if (f.security === "reality") {
    ss.realitySettings = {
      serverName: f.sni,
      fingerprint: f.fingerprint || "chrome",
      publicKey: f.publicKey,
      shortId: f.shortId,
      spiderX: f.spiderX,
    }
  }
  if (f.transport === "tcp" && f.security === "none") return undefined
  return ss
}

/** The xray outbound JSON for a form. */
export function buildConfig(f: BasicForm): Obj {
  const port = Number(f.port)
  const addr: Obj = { address: f.address.trim(), port }
  let settings: Obj
  switch (f.protocol) {
    case "vless":
      settings = { ...addr, id: f.id.trim(), encryption: "none", ...(f.flow && { flow: f.flow }) }
      break
    case "vmess":
      settings = { ...addr, id: f.id.trim(), security: f.vmessSecurity || "auto" }
      break
    case "trojan":
      settings = { servers: [{ ...addr, password: f.password }] }
      break
    case "shadowsocks":
      settings = { servers: [{ ...addr, method: f.method, password: f.password }] }
      break
    default: {
      const server: Obj = { ...addr }
      if (f.user) server.users = [{ user: f.user, pass: f.pass }]
      settings = { servers: [server] }
    }
  }
  const cfg: Obj = { protocol: f.protocol, settings }
  const ss = stream(f)
  if (ss) cfg.streamSettings = ss
  return cfg
}

const asObj = (v: unknown): Obj => (v && typeof v === "object" && !Array.isArray(v) ? (v as Obj) : {})
const str = (v: unknown) => (typeof v === "string" ? v : v == null ? "" : String(v))

/** Reads a stored config back into the form (best effort; see `roundTrips`). */
export function parseConfig(cfg: Obj): BasicForm | undefined {
  const protocol = str(cfg.protocol) as Protocol
  if (!protocols.some((p) => p.value === protocol)) return undefined
  const f = emptyForm(protocol)
  const st = asObj(cfg.settings)
  const server = asObj(Array.isArray(st.servers) ? st.servers[0] : st.vnext ? asObj(Array.isArray(st.vnext) ? st.vnext[0] : st.vnext) : st)
  f.address = str(server.address)
  f.port = Number(server.port) || 443
  switch (protocol) {
    case "vless":
      f.id = str(st.id)
      f.flow = str(st.flow)
      break
    case "vmess":
      f.id = str(st.id)
      f.vmessSecurity = str(st.security) || "auto"
      break
    case "trojan":
      f.password = str(server.password)
      break
    case "shadowsocks":
      f.password = str(server.password)
      f.method = str(server.method)
      break
    default: {
      const u = asObj(Array.isArray(server.users) ? server.users[0] : undefined)
      f.user = str(u.user)
      f.pass = str(u.pass)
    }
  }
  const ss = asObj(cfg.streamSettings)
  f.transport = (str(ss.network) || "tcp") as Transport
  f.security = (str(ss.security) || "none") as Security
  const ws = asObj(ss.wsSettings)
  const hu = asObj(ss.httpupgradeSettings)
  const xh = asObj(ss.xhttpSettings)
  const gr = asObj(ss.grpcSettings)
  f.path = str(ws.path || hu.path || xh.path) || "/"
  f.host = str(asObj(ws.headers).Host || hu.host || xh.host)
  f.serviceName = str(gr.serviceName)
  f.mode = gr.multiMode ? "multi" : str(xh.mode)
  const tls = asObj(ss.tlsSettings)
  const re = asObj(ss.realitySettings)
  f.sni = str(tls.serverName || re.serverName)
  f.fingerprint = str(tls.fingerprint || re.fingerprint)
  f.alpn = Array.isArray(tls.alpn) ? tls.alpn.join(",") : ""
  f.allowInsecure = tls.allowInsecure === true
  f.publicKey = str(re.publicKey)
  f.shortId = str(re.shortId)
  f.spiderX = str(re.spiderX)
  return f
}

const canon = (v: unknown): string =>
  JSON.stringify(v, (_k, x) =>
    x && typeof x === "object" && !Array.isArray(x)
      ? Object.fromEntries(Object.entries(x as Obj).sort(([a], [b]) => a.localeCompare(b)))
      : x,
  )

/**
 * True when the form can represent the config without losing anything: such configs open on the
 * basics tab, everything else (extra fields, unusual transports) opens on the JSON tab.
 */
export function roundTrips(cfg: Obj): boolean {
  const f = parseConfig(cfg)
  if (!f) return false
  const { tag: _tag, ...rest } = cfg
  void _tag
  return canon(buildConfig(f)) === canon(rest)
}

/** Why a form cannot be submitted yet, or undefined when it is complete. */
export function formProblem(f: BasicForm): string | undefined {
  if (!f.address.trim()) return "请填写服务器地址"
  const port = Number(f.port)
  if (!Number.isInteger(port) || port < 1 || port > 65535) return "端口应为 1-65535"
  if ((f.protocol === "vless" || f.protocol === "vmess") && !f.id.trim()) return "请填写 UUID"
  if ((f.protocol === "trojan" || f.protocol === "shadowsocks") && !f.password) return "请填写密码"
  if (f.security === "reality" && !f.publicKey.trim()) return "Reality 需要填写公钥(publicKey)"
  return undefined
}
