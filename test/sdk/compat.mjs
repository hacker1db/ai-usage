// Drives a live ai-gateway with the official OpenAI and Anthropic SDKs, the way Caido and other
// tools do: base URL + gateway key. Started by test/integration (GATEWAY_URL, GATEWAY_KEY).
import assert from "node:assert/strict";
import Anthropic from "@anthropic-ai/sdk";
import OpenAI from "openai";

const baseURL = process.env.GATEWAY_URL;
const apiKey = process.env.GATEWAY_KEY;
assert.ok(baseURL && apiKey, "GATEWAY_URL and GATEWAY_KEY are required");

const openai = new OpenAI({ baseURL: `${baseURL}/v1`, apiKey, maxRetries: 0 });
const anthropic = new Anthropic({ baseURL, apiKey, maxRetries: 0 });
const results = [];

async function check(name, fn) {
  try {
    await fn();
    results.push(`ok   ${name}`);
  } catch (err) {
    results.push(`FAIL ${name}: ${err?.message ?? err}`);
  }
}

await check("openai models.list", async () => {
  const ids = [];
  for await (const m of openai.models.list()) ids.push(m.id);
  assert.deepEqual(ids.sort(), ["claude-sonnet", "gpt-6-luna"]);
});

for (const model of ["gpt-6-luna", "claude-sonnet"]) {
  await check(`openai chat.completions ${model}`, async () => {
    const res = await openai.chat.completions.create({ model, messages: [{ role: "user", content: "ping" }] });
    assert.equal(res.choices[0].message.content, "pong");
    assert.ok(res.usage.prompt_tokens > 0);
  });
  await check(`openai chat.completions stream ${model}`, async () => {
    const stream = await openai.chat.completions.create({
      model, stream: true, stream_options: { include_usage: true }, messages: [{ role: "user", content: "ping" }],
    });
    let text = "";
    let usage;
    for await (const chunk of stream) {
      text += chunk.choices[0]?.delta?.content ?? "";
      usage = chunk.usage ?? usage;
    }
    assert.match(text, /^Hello from/);
    assert.ok(usage?.completion_tokens > 0, "final usage chunk");
  });

  await check(`anthropic messages ${model}`, async () => {
    const res = await anthropic.messages.create({ model, max_tokens: 64, messages: [{ role: "user", content: "ping" }] });
    assert.equal(res.content[0].text, "pong");
    assert.equal(res.stop_reason, "end_turn");
  });
  await check(`anthropic messages stream ${model}`, async () => {
    const stream = anthropic.messages.stream({ model, max_tokens: 64, messages: [{ role: "user", content: "ping" }] });
    const final = await stream.finalMessage();
    const text = final.content.filter((b) => b.type === "text").map((b) => b.text).join("");
    assert.match(text, /^Hello from/);
    assert.ok(final.usage.output_tokens > 0);
  });
}

await check("anthropic rejects a bad key", async () => {
  const bad = new Anthropic({ baseURL, apiKey: "aigw_wrong", maxRetries: 0 });
  await assert.rejects(bad.messages.create({ model: "gpt-6-luna", max_tokens: 5, messages: [{ role: "user", content: "x" }] }),
    (err) => err.status === 401);
});

console.log(results.join("\n"));
if (results.some((r) => r.startsWith("FAIL"))) process.exit(1);
