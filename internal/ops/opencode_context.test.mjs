import assert from "node:assert/strict"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { test } from "node:test"
import plugin, { bounded, clean, readResponse, requestCapture, requestText, responseText } from "./opencode_context.mjs"

test("uses the actual request text in order without protocol fields", () => {
  const text = requestText({
    model: "hidden", temperature: 0.7, tools: [{ name: "schema-only" }],
    messages: [
      { role: "system", content: "exact system\n  text" },
      { role: "user", content: [{ type: "text", text: "remember?" }] },
      { role: "assistant", tool_calls: [{ id: "protocol-id", function: { name: "lookup", arguments: '{"q":"a"}' } }] },
      { role: "tool", tool_call_id: "protocol-id", content: "actual result" },
    ],
  })
  assert.equal(text, 'system:\nexact system\n  text\n\nuser:\nremember?\n\nTool call:\nlookup\n{"q":"a"}\n\ntool:\nactual result')
  assert.match(requestText({ previous_response_id: "opaque", input: "next" }), /earlier context.*\n\nuser:\nnext/s)
})

test("decodes real SSE text and tool deltas without duplicating them", async () => {
  const delta = value => `data: ${JSON.stringify({ choices: [{ index: 0, delta: value }] })}\r\n\r\n`
  const stream = delta({ content: "你" }) + delta({ content: "好" }) +
    delta({ tool_calls: [{ index: 0, function: { name: "search", arguments: '{"q"' } }] }) +
    delta({ tool_calls: [{ index: 0, function: { arguments: ':"x"}' } }] }) + "data: [DONE]"
  const bytes = new TextEncoder().encode(stream)
  const response = new Response(new ReadableStream({
    start(controller) {
      for (let i = 0; i < bytes.length; i++) controller.enqueue(bytes.subarray(i, i + 1))
      controller.close()
    },
  }), { headers: { "content-type": "text/event-stream" } })
  assert.deepEqual(await readResponse(response, new AbortController().signal), {
    text: 'assistant:\n你好\n\nTool call:\nsearch\n{"q":"x"}', incomplete: false,
  })
})

test("only complete actual request snapshots have ordered message blocks", () => {
  const capture = requestCapture({ input: [
    { type: "message", role: "developer", content: "instructions" },
    { type: "message", role: "user", content: [{ type: "input_text", text: "question" }] },
    { type: "function_call", name: "look", arguments: "{}", call_id: "opaque" },
    { type: "function_call_output", call_id: "opaque", output: "result" },
  ] })
  assert.equal(capture.incomplete, false)
  assert.deepEqual(capture.blocks.map(({ role, text }) => [role, text]), [
    ["developer", "instructions"], ["user", "question"], ["tool_call", "look\n{}"], ["tool_result", "result"],
  ])
  assert.doesNotMatch(capture.text, /opaque/)
  for (const body of [
    { previous_response_id: "res_opaque", input: "more" },
    { conversation: "conv_opaque", input: "more" },
    { input: [{ type: "item_reference", id: "opaque" }, { role: "user", content: "more" }] },
  ]) {
    const delta = requestCapture(body)
    assert.equal(delta.incomplete, true)
    assert.equal(delta.blocks, undefined)
    assert.match(delta.text, /references earlier context/)
    assert.doesNotMatch(delta.text, /opaque/)
  }
  const image = requestCapture({ messages: [{ role: "user", content: [{ type: "image_url", image_url: "data:opaque" }] }] })
  assert.deepEqual(image, { text: "user:\n[non-text attachment]", incomplete: true })
})

test("keeps final Responses output once and marks partial streams", () => {
  const capture = responseText()
  capture.frame(JSON.stringify({ type: "response.output_text.delta", delta: "one" }))
  assert.deepEqual(capture.result(), { text: "assistant:\none", incomplete: true })
  capture.frame(JSON.stringify({ type: "response.completed", response: { output: [
    { role: "assistant", content: [{ type: "output_text", text: "one" }] },
    { type: "function_call", name: "read", arguments: "{}" },
  ] } }))
  assert.deepEqual(capture.result(), { text: "assistant:\none\n\nTool call:\nread\n{}", incomplete: false })
})

test("redacts credential values before UTF-8 truncation", () => {
  const payload = bounded(clean('Bearer token123 {"password":"value123"} known-value ' + "好".repeat(3000), ["known-value"]))
  assert.equal(payload.truncated, true)
  assert.ok(new TextEncoder().encode(payload.text).length <= 8192)
  assert.doesNotMatch(payload.text, /token123|value123|known-value|\uFFFD/)
})

test("HTTP hooks leave the bodies readable and publish one paired call", async () => {
  const dir = await mkdtemp(join(tmpdir(), "zoe-opencode-context-"))
  const token = join(dir, "token")
  const oldFetch = globalThis.fetch
  const saved = { ZOE_OPENCODE_OBSERVATION_URL: process.env.ZOE_OPENCODE_OBSERVATION_URL, ZOE_RUNNER_TOKEN_FILE: process.env.ZOE_RUNNER_TOKEN_FILE }
  let cleanup, timer
  try {
    await writeFile(token, "fixture-only")
    process.env.ZOE_OPENCODE_OBSERVATION_URL = "https://example.invalid"
    process.env.ZOE_RUNNER_TOKEN_FILE = token
    const hooks = new Map(), sent = []
    let deliver, count = 2
    const result = new Promise(resolve => { deliver = resolve })
    globalThis.fetch = async (_url, options) => {
      if (options.method !== "POST") return Response.json({ boot: "boot", generation: 1, run_id: "run", attempt_id: "attempt", fence: 1, conversation_metadata: true })
      sent.push(JSON.parse(options.body))
      if (sent.length === count) deliver([...sent])
      return new Response(null, { status: 204 })
    }
    cleanup = await plugin.setup({ session: { hook: (name, callback) => hooks.set(name, callback) } })
    const body = '{"messages":[{"role":"user","content":"question"}]}'
    const output = '{"choices":[{"message":{"role":"assistant","content":"answer"}}]}'
    const request = new Request("https://example.invalid/v1/chat/completions", { method: "POST", body })
    const response = new Response(output)
    const event = { request, sessionID: "session", kind: "primary", model: { providerID: "test", id: "model" } }
    assert.equal(hooks.get("http.request")(event), undefined)
    assert.equal(hooks.get("http.response")({ ...event, response }), undefined)
    const [input, reply] = await Promise.race([result, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error("observation timed out")), 2000) })])
    assert.deepEqual([input.type, reply.type, input.text, reply.text], ["request", "response", "user:\nquestion", "assistant:\nanswer"])
    assert.equal(input.call_id, reply.call_id)
    assert.match(input.conversation_id, /^[a-f0-9]{64}$/)
    assert.equal(input.conversation_id, reply.conversation_id)
    assert.equal(input.conversation_start, undefined)
    assert.deepEqual(input.request_messages, [{ role: "user", text: "question" }])
    assert.equal(await request.text(), body)
    assert.equal(await response.text(), output)

    count = 4
    const continuation = new Promise(resolve => { deliver = resolve })
    const frame = JSON.stringify({ type: "response.create", previous_response_id: "res_fixture", input: "follow-up" })
    hooks.get("experimental.ws.send")({ ...event, frame })
    hooks.get("experimental.ws.receive")({ ...event, frame: JSON.stringify({ type: "response.completed", response: { output: [] } }) })
    const [, , delta, deltaReply] = await Promise.race([continuation, new Promise((_, reject) => {
      clearTimeout(timer)
      timer = setTimeout(() => reject(new Error("continuation timed out")), 2000)
    })])
    assert.equal(delta.conversation_id, input.conversation_id)
    assert.equal(delta.call_id, deltaReply.call_id)
    assert.notEqual(delta.call_id, input.call_id)
    assert.equal(delta.incomplete, true)
    assert.equal(delta.truncated, false)
    assert.equal(delta.request_messages, undefined)
    assert.match(delta.text, /references earlier context.*follow-up/s)
    assert.doesNotMatch(delta.text, /res_fixture/)
  } finally {
    clearTimeout(timer)
    cleanup?.()
    globalThis.fetch = oldFetch
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[key]
      else process.env[key] = value
    }
    await rm(dir, { recursive: true, force: true })
  }
})
