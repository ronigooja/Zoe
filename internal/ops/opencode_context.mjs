import { createHash, randomBytes } from "node:crypto"
import { readFile } from "node:fs/promises"

const textLimit = 8192
const scanLimit = 512 * 1024
const queueLimit = 32
const encoder = new TextEncoder()
const roles = new Set(["system", "developer", "user", "assistant", "tool", "function"])

// Match the Worker monitor's credential filtering before text leaves OpenCode.
export function clean(text, secrets) {
  for (const secret of secrets) if (secret) text = text.split(secret).join("[redacted]")
  return text.replace(/(bearer\s+|\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization)\b["'\s]*[:=]["'\s]*)[^\s"',;]+|\b(?:sk|ghp|github_pat)_[a-z0-9_-]{8,}\b|\bsk-[a-z0-9_-]{8,}\b/gi, "[redacted]")
}

export function bounded(text) {
  const bytes = encoder.encode(text)
  if (bytes.length <= textLimit) return { text, truncated: false }
  let end = textLimit
  while ((bytes[end] & 0xc0) === 0x80) end--
  return { text: new TextDecoder().decode(bytes.subarray(0, end)), truncated: true }
}

function textOf(value) {
  if (typeof value === "string") return value
  if (!Array.isArray(value)) return ""
  return value.flatMap(part => {
    if (["text", "input_text", "output_text", "refusal"].includes(part?.type)) {
      const text = part.text ?? part.refusal
      return typeof text === "string" ? [text] : []
    }
    if (["image", "image_url", "input_image", "input_file", "file", "input_audio"].includes(part?.type)) return ["[non-text attachment]"]
    return []
  }).join("\n")
}

function toolText(name, args) {
  if (typeof name !== "string" && typeof args !== "string") return ""
  return [name, args].filter(v => typeof v === "string" && v !== "").join("\n")
}

function sections(items) {
  const blocks = []
  for (const item of items) {
    if (!item || typeof item !== "object") continue
    if (item.type === "function_call" || item.type === "custom_tool_call") {
      blocks.push(`Tool call:\n${toolText(item.name, item.arguments ?? item.input)}`)
    } else if (item.type === "function_call_output" || item.type === "custom_tool_call_output") {
      blocks.push(`Tool result:\n${textOf(item.output)}`)
    } else if (roles.has(item.role)) {
      const text = textOf(item.content)
      if (text || item.role === "tool" || item.role === "function") blocks.push(`${item.role}:\n${text}`)
      for (const call of Array.isArray(item.tool_calls) ? item.tool_calls : []) {
        const text = toolText(call?.function?.name, call?.function?.arguments)
        if (text) blocks.push(`Tool call:\n${text}`)
      }
      if (item.function_call) {
        const text = toolText(item.function_call.name, item.function_call.arguments)
        if (text) blocks.push(`Tool call:\n${text}`)
      }
    }
  }
  return blocks.join("\n\n")
}

export function requestText(body) {
  if (!body || typeof body !== "object") return undefined
  const reference = referencesContext(body) ? "[This request references earlier context not sent in this call.]" : ""
  if (Array.isArray(body.messages)) return [reference, sections(body.messages)].filter(Boolean).join("\n\n")
  if (typeof body.input !== "string" && !Array.isArray(body.input)) return undefined
  const instructions = typeof body.instructions === "string" && body.instructions !== "" ? `system:\n${body.instructions}` : ""
  const input = typeof body.input === "string" ? `user:\n${body.input}` : sections(body.input)
  return [reference, instructions, input].filter(Boolean).join("\n\n")
}

function referencesContext(body) {
  return body.previous_response_id != null || body.conversation != null ||
    (Array.isArray(body.input) && body.input.some(item => item?.type === "item_reference"))
}

function structuredText(value) {
  if (typeof value === "string") return value
  if (!Array.isArray(value)) return undefined
  const parts = []
  for (const part of value) {
    if (!part || !["text", "input_text", "output_text", "refusal"].includes(part.type)) return undefined
    const text = part.text ?? part.refusal
    if (typeof text !== "string") return undefined
    parts.push(text)
  }
  return parts.join("\n")
}

// Derive ordered role/text blocks only from the actual outbound JSON. A
// reference-only delta, unsupported item, or attachment is not a full snapshot.
export function requestCapture(body) {
  const text = requestText(body)
  if (text === undefined || referencesContext(body)) return { text, incomplete: true }
  const blocks = []
  const add = (role, text, label = role) => blocks.push({ role, text, label })
  const partial = () => ({ text, incomplete: true })
  function addCall(call, args) {
    if (typeof call?.name !== "string" || typeof args !== "string") return false
    add("tool_call", toolText(call.name, args), "Tool call")
    return true
  }
  const chat = Array.isArray(body.messages)
  if (chat && (body.input != null || body.instructions != null)) return partial()
  if (!chat && body.instructions != null) {
    if (typeof body.instructions !== "string") return partial()
    if (body.instructions !== "") add("system", body.instructions)
  }
  const items = chat ? body.messages : body.input
  if (typeof items === "string") {
    add("user", items)
  } else for (const item of items) {
    if (!item || typeof item !== "object") return partial()
    if (!chat && (item.type === "function_call" || item.type === "custom_tool_call")) {
      if (!addCall(item, item.arguments ?? item.input)) return partial()
    } else if (!chat && (item.type === "function_call_output" || item.type === "custom_tool_call_output")) {
      const result = structuredText(item.output)
      if (result === undefined) return partial()
      add("tool_result", result, "Tool result")
    } else {
      if (!roles.has(item.role) || (item.type != null && item.type !== "message") || item.audio != null || item.refusal != null ||
          item.reasoning_content != null || item.reasoning_details != null || item.reasoning_text != null || item.reasoning != null ||
          (item.tool_calls != null && !Array.isArray(item.tool_calls))) return partial()
      const hasCalls = item.tool_calls?.length || item.function_call != null
      const content = item.content == null && hasCalls ? "" : structuredText(item.content)
      if (content === undefined) return partial()
      if (content || item.role === "tool" || item.role === "function") add(item.role, content)
      else if (!hasCalls) return partial()
      for (const call of item.tool_calls ?? []) {
        if (call?.type != null && call.type !== "function") return partial()
        if (!addCall(call?.function, call?.function?.arguments)) return partial()
      }
      if (item.function_call != null && !addCall(item.function_call, item.function_call.arguments)) return partial()
    }
  }
  // The additional representation cannot reveal text missing from the
  // established payload. Empty or ambiguously lowered requests stay unknown.
  if (!blocks.length || flattenBlocks(blocks) !== text) return partial()
  return { text, blocks, incomplete: false }
}

const flattenBlocks = blocks => blocks.map(({ label, text }) => `${label}:\n${text}`).join("\n\n")

function safeRequestMessages(blocks, text, secrets) {
  if (!blocks) return undefined
  const cleaned = blocks.map(block => ({ ...block, text: clean(block.text, secrets) }))
  // A credential matched across block boundaries must not reappear in the
  // per-message version. Count both representations against the same budget.
  if (flattenBlocks(cleaned) !== text) return undefined
  const messages = cleaned.map(({ role, text }) => ({ role, text }))
  const bytes = encoder.encode(JSON.stringify({ text })).length +
    encoder.encode(JSON.stringify({ request_messages: messages })).length - 1
  return bytes <= textLimit ? messages : undefined
}

function finalText(body) {
  if (Array.isArray(body?.output)) return sections(body.output)
  if (Array.isArray(body?.choices) && body.choices.some(choice => choice?.message)) {
    return sections(body.choices.flatMap(choice => choice?.message ? [choice.message] : []))
  }
  return undefined
}

function incompleteFinal(body) {
  if (body?.status === "incomplete" || body?.status === "failed") return true
  if (Array.isArray(body?.output) && body.output.length > 0) return requestCapture({ input: body.output }).incomplete
  return Array.isArray(body?.choices) && body.choices.some(choice =>
    ["length", "content_filter"].includes(choice?.finish_reason) || (choice?.message && requestCapture({ messages: [choice.message] }).incomplete))
}

export function responseText() {
  const entries = new Map()
  let final, terminal = false, supported = false, failed = false
  function add(id, field, value, kind) {
    if (typeof value !== "string" || value === "") return
    const part = entries.get(id) ?? { kind, text: "", name: "", args: "" }
    part[field] += value
    entries.set(id, part)
  }
  return {
    frame(payload) {
      if (payload === "[DONE]") { terminal = true; return }
      let body
      try { body = JSON.parse(payload) } catch { failed = true; return }
      const direct = finalText(body)
      if (direct !== undefined) {
        terminal = supported = true
        final = direct
        failed ||= incompleteFinal(body)
        return
      }
      if (Array.isArray(body?.choices)) {
        supported = true
        for (const choice of body.choices) {
          const delta = choice?.delta
          const id = choice?.index ?? 0
          if (choice?.finish_reason != null) terminal = true
          if (["length", "content_filter"].includes(choice?.finish_reason)) failed = true
          add(`${id}:text`, "text", textOf(delta?.content) || delta?.refusal, "assistant")
          for (const call of Array.isArray(delta?.tool_calls) ? delta.tool_calls : []) {
            const key = `${id}:tool:${call.index ?? 0}`
            add(key, "name", call.function?.name, "tool")
            add(key, "args", call.function?.arguments, "tool")
          }
          add(`${id}:function`, "name", delta?.function_call?.name, "tool")
          add(`${id}:function`, "args", delta?.function_call?.arguments, "tool")
        }
        return
      }
      const id = body?.output_index ?? 0
      switch (body?.type) {
        case "response.output_text.delta":
        case "response.refusal.delta":
          supported = true
          add(`${id}:text:${body.content_index ?? 0}`, "text", body.delta, "assistant")
          break
        case "response.output_item.added":
          supported = true
          if (["function_call", "custom_tool_call"].includes(body.item?.type)) add(`${id}:tool`, "name", body.item.name, "tool")
          break
        case "response.function_call_arguments.delta":
        case "response.custom_tool_call_input.delta":
          supported = true
          add(`${id}:tool`, "args", body.delta, "tool")
          break
        case "response.completed":
        case "response.incomplete":
          terminal = true
          if (body.type === "response.incomplete" || incompleteFinal(body.response)) failed = true
          final = finalText(body.response)
          if (final !== undefined) supported = true
          break
        case "response.failed":
        case "error":
          failed = terminal = true
          break
      }
    },
    done() { return terminal },
    result() {
      const text = final ?? [...entries.values()].map(p => p.kind === "tool" ? `Tool call:\n${toolText(p.name, p.args)}` : `assistant:\n${p.text}`).join("\n\n")
      return { text, incomplete: failed || !supported || !terminal }
    },
  }
}

// Read an observer-only clone, with bounded work. Never await cancellation of
// one tee branch: it can wait for the model consumer, which uses the other one.
async function readBody(stream, signal, consume, fatal = false) {
  if (!stream) return true
  const reader = stream.getReader()
  const stop = () => { void reader.cancel().catch(() => {}) }
  signal.addEventListener("abort", stop, { once: true })
  const decoder = new TextDecoder("utf-8", { fatal })
  let seen = 0
  try {
    if (signal.aborted) return false
    for (;;) {
      const { value, done } = await reader.read()
      if (done) { consume(decoder.decode()); return !signal.aborted }
      seen += value.byteLength
      if (seen > scanLimit) return "limit"
      consume(decoder.decode(value, { stream: true }))
    }
  } catch {
    return false
  } finally {
    signal.removeEventListener("abort", stop)
    stop()
  }
}

export async function readResponse(response, signal) {
  const output = responseText()
  const isSSE = response.headers.get("content-type")?.includes("text/event-stream")
  let buffer = "", data = []
  const consume = chunk => {
    buffer += chunk
    if (!isSSE) return
    for (;;) {
      const index = buffer.indexOf("\n")
      if (index < 0) break
      const line = buffer.slice(0, index).replace(/\r$/, "")
      buffer = buffer.slice(index + 1)
      if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""))
      if (!line && data.length) { output.frame(data.join("\n")); data = [] }
    }
  }
  const complete = await readBody(response.body, signal, consume, true)
  if (!isSSE && complete === true) output.frame(buffer)
  if (isSSE && complete === true) consume("\n\n")
  const result = output.result()
  if (complete === "limit") return { ...result, incomplete: true, truncated: true }
  return { ...result, incomplete: !complete || result.incomplete }
}

const id = () => randomBytes(16).toString("hex")
const socketKey = event => JSON.stringify([event.sessionID, event.kind, event.model.providerID, event.model.id])
// The native hooks identify the primary conversation, but not its first send
// or which auxiliary sends are retries of the same auxiliary request.
const conversationID = (session, kind) => kind === "primary" ? createHash("sha256").update(session).digest("hex") : undefined

export default {
  id: "zoe-opencode-context",
  async setup(ctx) {
    const base = process.env.ZOE_OPENCODE_OBSERVATION_URL
    const tokenFile = process.env.ZOE_RUNNER_TOKEN_FILE
    if (!base || !tokenFile) return
    const token = (await readFile(tokenFile, "utf8").catch(() => "")).trim()
    if (!token) return
    const secrets = [token, process.env.OPENCODE_SERVER_PASSWORD, process.env.ZOE_PROVIDER_KEY]
    const stop = new AbortController()
    const http = new WeakMap()
    const sockets = new Map()
    const leases = new Map()
    const queue = []
    let sending = false
    let origin
    try {
      const url = new URL(base)
      if (!["https:", "http:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) return
      origin = url.href.replace(/\/$/, "")
    } catch { return }
    const endpoint = session => `${origin}/runner/v1/opencode/sessions/${encodeURIComponent(session)}/model-observation`
    const signal = (ms = 1000) => AbortSignal.any([stop.signal, AbortSignal.timeout(ms)])
    function lease(session) {
      const now = Date.now()
      const prior = leases.get(session)
      if (prior && now < prior.expires) return prior.pending
      if (leases.size >= 64) leases.delete(leases.keys().next().value)
      const pending = fetch(endpoint(session), { headers: { Authorization: `Bearer ${token}` }, signal: signal(), redirect: "error" }).then(async response => {
        if (!response.ok) { void response.body?.cancel().catch(() => {}); return undefined }
        const value = await response.json()
        const { generation, boot, run_id, attempt_id, fence, conversation_metadata } = value
        return generation && boot && run_id && attempt_id && fence ? { fields: { generation, boot, run_id, attempt_id, fence }, conversationMetadata: conversation_metadata === true } : undefined
      }).catch(() => undefined)
      leases.set(session, { expires: now + 750, pending })
      return pending
    }
    async function drain() {
      if (sending) return
      sending = true
      try {
        while (queue.length && !stop.signal.aborted) {
          const { session, body } = queue.shift()
          const response = await fetch(endpoint(session), {
            method: "POST", headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
            body: JSON.stringify(body), signal: signal(), redirect: "error",
          }).catch(() => undefined)
          void response?.body?.cancel().catch(() => {})
        }
      } finally { sending = false }
    }
    function publish(call, type, value, incomplete = false, blocks, scanTruncated = false) {
      if (!call || queue.length >= queueLimit || stop.signal.aborted) return
      const payload = bounded(clean(value || (incomplete ? "[Model content unavailable]" : ""), secrets))
      const metadata = call.lease.conversationMetadata
      const body = { ...call.lease.fields, call_id: call.id, type, text: payload.text, truncated: payload.truncated || scanTruncated || (!metadata && incomplete) }
      if (metadata) {
        if (call.conversation) body.conversation_id = call.conversation
        if (incomplete) body.incomplete = true
        if (type === "request" && !incomplete && !body.truncated) {
          const messages = safeRequestMessages(blocks, payload.text, secrets)
          if (messages) body.request_messages = messages
        }
      }
      queue.push({ session: call.session, body })
      void drain()
    }
    function startCall(session, kind, lease) {
      return { id: id(), lease, session, conversation: conversationID(session, kind) }
    }
    function publishRequest(call, captured) {
      publish(call, "request", captured?.text, captured?.incomplete ?? true, captured?.blocks, captured?.truncated)
    }
    let readers = 0
    function observeRead(work) {
      if (readers >= queueLimit) return
      readers++
      void Promise.resolve().then(work).catch(() => {}).finally(() => { readers-- })
    }
    await ctx.session.hook("http.request", event => {
      try {
        if (readers >= queueLimit) return
        const { request, sessionID } = event
        let resolve
        const ready = new Promise(done => { resolve = done })
        http.set(request, ready)
        observeRead(async () => {
          let call
          try {
            const active = await lease(sessionID)
            if (!active) return
            call = startCall(sessionID, event.kind, active)
            let body = "", value
            try {
              const complete = await readBody(request.clone().body, signal(), text => { body += text }, true)
              if (complete === true) value = requestCapture(JSON.parse(body))
              if (complete === "limit") value = { incomplete: true, truncated: true }
            } catch {}
            publishRequest(call, value)
          } finally { resolve(call) }
        })
      } catch {} // Observation must not alter dispatch.
    })
    await ctx.session.hook("http.response", event => {
      try {
        const ready = http.get(event.request)
        if (!ready) return
        http.delete(event.request)
        if (readers >= queueLimit || event.response.status >= 400) {
          void ready.then(call => publish(call, "response", "[Model response unavailable]", true)).catch(() => {})
          return
        }
        const response = event.response.clone()
        observeRead(async () => {
          const call = await ready
          if (!call) { void response.body?.cancel().catch(() => {}); return }
          const result = await readResponse(response, signal(600_000))
          publish(call, "response", result.text, result.incomplete, undefined, result.truncated)
        })
      } catch {}
    })
    function finishSocket(call, incomplete, truncated = false) {
      const result = call.output.result()
      void call.ready.then(active => publish(active, "response", result.text, incomplete || result.incomplete, undefined, truncated)).catch(() => {})
    }
    await ctx.session.hook("experimental.ws.send", event => {
      try {
        const key = socketKey(event)
        const previous = sockets.get(key)
        if (previous) { finishSocket(previous, true); sockets.delete(key) }
        if (sockets.size >= 32) {
          const oldest = sockets.keys().next().value
          const lost = sockets.get(oldest)
          finishSocket(lost, true)
          sockets.delete(oldest)
        }
        const frame = event.frame.length <= scanLimit ? event.frame : undefined
        const session = event.sessionID
        const ready = lease(session).then(active => {
          if (!active) return
          const call = startCall(session, event.kind, active)
          let value
          if (frame !== undefined && encoder.encode(frame).length <= scanLimit) {
            try { value = requestCapture(JSON.parse(frame)) } catch {}
          } else value = { incomplete: true, truncated: true }
          publishRequest(call, value)
          return call
        }).catch(() => undefined)
        sockets.set(key, { ready, output: responseText(), bytes: 0 })
      } catch {}
    })
    await ctx.session.hook("experimental.ws.receive", event => {
      try {
        const key = socketKey(event)
        const call = sockets.get(key)
        if (!call) return
        call.bytes += encoder.encode(event.frame).length
        const oversized = call.bytes > scanLimit
        if (!oversized) call.output.frame(event.frame)
        if (!oversized && !call.output.done()) return
        sockets.delete(key)
        finishSocket(call, oversized, oversized)
      } catch {}
    })
    return () => { stop.abort(); queue.length = 0; leases.clear(); sockets.clear() }
  },
}
