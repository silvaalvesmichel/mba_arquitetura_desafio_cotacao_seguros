// Collects evidence from the running compose environment: Prometheus range queries rendered as SVG
// charts (with the PromQL and the time window printed inside the image) and Jaeger traces exported as
// JSON. Usage: node collect-evidence.mjs <outDir> <sinceEpochSeconds> <item>...
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const PROMETHEUS = process.env.PROMETHEUS_URL ?? "http://localhost:9090";
const JAEGER = process.env.JAEGER_URL ?? "http://localhost:16686";

const [outDir, sinceArg, ...items] = process.argv.slice(2);
if (!outDir || !sinceArg || items.length === 0) {
  console.error("usage: collect-evidence.mjs <outDir> <sinceEpochSeconds> <item>...");
  process.exit(2);
}
const since = Number(sinceArg);
const until = Math.floor(Date.now() / 1000);
const STEP = 5;
mkdirSync(outDir, { recursive: true });

const P95_API = "histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket[1m])))";

const charts = {
  "p95-post-quotes": {
    title: "p95 do POST /quotes (segundos)",
    query: P95_API,
    yMax: 10,
    unit: "s",
  },
  "p95-por-parceira-http": {
    title: "p95 por parceira, borda HTTP (segundos)",
    query: "histogram_quantile(0.95, sum by (server_address, le) (rate(http_client_request_duration_seconds_bucket[1m])))",
    yMax: 10,
    unit: "s",
    label: (m) => m.server_address,
  },
  "cache-hit-rate": {
    title: "Hit rate acumulado do cache (hit / total)",
    query: 'sum(quotation_cache_requests_total{result="hit"}) / sum(quotation_cache_requests_total)',
    yMax: 1,
    unit: "",
  },
  "breaker-estado": {
    title: "Estado do circuit breaker por parceira (0 fechado, 1 meio aberto, 2 aberto)",
    query: "quotation_breaker_state",
    yMax: 2,
    unit: "",
    stepped: true,
    ticks: [
      [0, "fechado"],
      [1, "meio aberto"],
      [2, "aberto"],
    ],
    label: (m) => m.partner,
  },
  "breaker-transicoes": {
    title: "Transições do circuit breaker (contador acumulado)",
    query: "sum by (partner, from, to) (quotation_breaker_transitions_total)",
    unit: "",
    stepped: true,
    label: (m) => `${m.partner}: ${m.from} -> ${m.to}`,
  },
  "latencia-por-parceira": {
    title: "p95 da latência por parceira, métrica de negócio (segundos)",
    query: "histogram_quantile(0.95, sum by (partner, le) (rate(quotation_partner_duration_seconds_bucket[1m])))",
    yMax: 3,
    unit: "s",
    label: (m) => m.partner,
  },
};

const COLORS = ["#1f77b4", "#d62728", "#2ca02c", "#ff7f0e", "#9467bd", "#8c564b", "#17becf", "#7f7f7f"];

const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
const clock = (epoch) => new Date(epoch * 1000).toISOString().slice(11, 19);

async function getJSON(url) {
  const response = await fetch(url);
  if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
  return response.json();
}

function wrap(text, width) {
  const lines = [];
  for (let rest = text; rest.length > 0; rest = rest.slice(width)) lines.push(rest.slice(0, width));
  return lines;
}

function render(name, spec, result) {
  const W = 1200;
  const H = 620;
  const left = 110;
  const right = 30;
  const top = 60;
  const plotH = 360;
  const plotW = W - left - right;

  const series = result.map((s) => ({
    name: spec.label ? spec.label(s.metric) : "valor",
    points: s.values.map(([t, v]) => [Number(t), Number(v)]).filter(([, v]) => Number.isFinite(v)),
  }));
  const observed = Math.max(0, ...series.flatMap((s) => s.points.map(([, v]) => v)));
  const yMax = spec.yMax ?? Math.max(1, Math.ceil(observed * 1.1));
  const x = (t) => left + ((t - since) / Math.max(1, until - since)) * plotW;
  const y = (v) => top + plotH - (Math.min(v, yMax) / yMax) * plotH;

  const out = [];
  out.push(`<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" font-family="DejaVu Sans, Arial, sans-serif">`);
  out.push(`<rect width="${W}" height="${H}" fill="#ffffff"/>`);
  out.push(`<text x="${left}" y="32" font-size="20" font-weight="bold">${esc(spec.title)}</text>`);

  const ticks = spec.ticks ?? [0, 0.25, 0.5, 0.75, 1].map((f) => [f * yMax, `${+(f * yMax).toFixed(2)}${spec.unit}`]);
  for (const [value, text] of ticks) {
    out.push(`<line x1="${left}" y1="${y(value)}" x2="${left + plotW}" y2="${y(value)}" stroke="#dddddd"/>`);
    out.push(`<text x="${left - 8}" y="${y(value) + 5}" font-size="13" text-anchor="end">${esc(text)}</text>`);
  }
  for (let i = 0; i <= 6; i++) {
    const t = since + ((until - since) * i) / 6;
    out.push(`<line x1="${x(t)}" y1="${top + plotH}" x2="${x(t)}" y2="${top + plotH + 5}" stroke="#333333"/>`);
    out.push(`<text x="${x(t)}" y="${top + plotH + 22}" font-size="13" text-anchor="middle">${clock(t)}</text>`);
  }
  out.push(`<rect x="${left}" y="${top}" width="${plotW}" height="${plotH}" fill="none" stroke="#333333"/>`);

  series.forEach((s, i) => {
    const color = COLORS[i % COLORS.length];
    let path = "";
    s.points.forEach(([t, v], j) => {
      const gap = j > 0 && t - s.points[j - 1][0] > STEP * 3;
      if (j === 0 || gap) path += `M${x(t).toFixed(1)},${y(v).toFixed(1)} `;
      else if (spec.stepped) path += `H${x(t).toFixed(1)} V${y(v).toFixed(1)} `;
      else path += `L${x(t).toFixed(1)},${y(v).toFixed(1)} `;
    });
    out.push(`<path d="${path}" fill="none" stroke="${color}" stroke-width="2"/>`);
    for (const [t, v] of s.points) out.push(`<circle cx="${x(t).toFixed(1)}" cy="${y(v).toFixed(1)}" r="2.5" fill="${color}"/>`);
    const lx = left + (i % 2) * 540;
    const ly = top + plotH + 50 + Math.floor(i / 2) * 20;
    const peak = s.points.length ? Math.max(...s.points.map(([, v]) => v)) : NaN;
    out.push(`<rect x="${lx}" y="${ly - 10}" width="14" height="10" fill="${color}"/>`);
    out.push(`<text x="${lx + 20}" y="${ly}" font-size="13">${esc(s.name)} (max ${Number.isFinite(peak) ? +peak.toFixed(3) : "sem dados"})</text>`);
  });
  if (series.length === 0) out.push(`<text x="${left + 20}" y="${top + 40}" font-size="16" fill="#aa0000">sem dados na janela</text>`);

  let footer = H - 62;
  for (const line of wrap(`PromQL: ${spec.query}`, 150)) {
    out.push(`<text x="20" y="${footer}" font-size="12" font-family="DejaVu Sans Mono, monospace">${esc(line)}</text>`);
    footer += 16;
  }
  out.push(
    `<text x="20" y="${H - 12}" font-size="12">Janela: ${new Date(since * 1000).toISOString()} a ${new Date(until * 1000).toISOString()} (UTC), passo ${STEP}s, /api/v1/query_range do Prometheus</text>`,
  );
  out.push("</svg>");
  writeFileSync(join(outDir, `${name}.svg`), out.join("\n"));
}

async function chart(name) {
  const spec = charts[name];
  const url = `${PROMETHEUS}/api/v1/query_range?query=${encodeURIComponent(spec.query)}&start=${since}&end=${until}&step=${STEP}`;
  const body = await getJSON(url);
  writeFileSync(
    join(outDir, `${name}.json`),
    JSON.stringify({ query: spec.query, start: since, end: until, step: STEP, result: body.data.result }, null, 1),
  );
  render(name, spec, body.data.result);
  const values = body.data.result.flatMap((s) => s.values.map(([, v]) => Number(v)).filter(Number.isFinite));
  console.log(`${name}: ${body.data.result.length} series, ${values.length} points, max ${values.length ? Math.max(...values) : "n/a"}`);
}

const tag = (span, key) => span.tags.find((t) => t.key === key)?.value;
const isClient = (span) => tag(span, "span.kind") === "client";
const root = (trace) => trace.spans.find((s) => s.operationName === "POST /quotes" && tag(s, "span.kind") === "server");

const SHOWN = /^(span\.kind|error|http\.response\.status_code|server\.address|url\.full|tenant_id|partner|quotation\.)/;

function describe(trace) {
  const spans = [...trace.spans].sort((a, b) => a.startTime - b.startTime);
  const start = spans[0].startTime;
  const lines = [`traceID ${trace.traceID} | ${spans.length} spans`];
  for (const span of spans) {
    const tags = span.tags
      .filter((t) => SHOWN.test(t.key))
      .map((t) => `${t.key}=${t.value}`)
      .join(" ");
    const events = (span.logs ?? [])
      .map((l) => l.fields.find((f) => f.key === "event")?.value)
      .filter(Boolean)
      .map((e) => ` event=${e}`)
      .join("");
    lines.push(
      `  +${((span.startTime - start) / 1000).toFixed(1).padStart(8)} ms  ${(span.duration / 1000).toFixed(1).padStart(8)} ms  ${span.operationName}  ${tags}${events}`,
    );
  }
  return lines.join("\n") + "\n";
}

function save(name, trace, caption) {
  if (!trace) {
    console.log(`${name}: NOT FOUND`);
    return;
  }
  writeFileSync(join(outDir, `${name}.json`), JSON.stringify({ data: [trace] }));
  writeFileSync(join(outDir, `${name}.txt`), `${caption}\n${describe(trace)}`);
  console.log(`${name}: trace ${trace.traceID}`);
}

async function loadTraces() {
  const url = `${JAEGER}/api/traces?service=quotation-api&start=${since * 1e6}&end=${(until + 60) * 1e6}&limit=5000`;
  const traces = (await getJSON(url)).data.filter(root);
  console.log(`jaeger: ${traces.length} traces of POST /quotes`);
  return traces;
}

async function tracesBefore() {
  const traces = await loadTraces();
  const complete = traces.filter((t) => t.spans.filter(isClient).length === 3);
  const slowest = complete.sort((a, b) => root(b).duration - root(a).duration)[0];
  const failed = traces.find((t) => String(tag(root(t), "http.response.status_code")) === "502");
  save("trace-antes-lento", slowest, "Trace mais longo do antes: tres chamadas em serie, uma colada na outra.");
  save("trace-antes-erro", failed, "Trace com 502 do antes: a sequencia morre na parceira que falhou.");
}

async function tracesAfter() {
  const traces = await loadTraces();
  const lookups = (t) => t.spans.filter((s) => s.operationName === "partner.quote");
  const shortCircuited = (s) => String(tag(s, "quotation.breaker.short_circuited")) === "true";

  const open = traces
    .filter((t) => lookups(t).some(shortCircuited))
    .sort((a, b) => root(a).duration - root(b).duration)[0];
  const cached = traces.find(
    (t) =>
      lookups(t).length === 3 &&
      lookups(t).every((s) => tag(s, "quotation.cache.result") === "hit") &&
      t.spans.filter(isClient).length === 0,
  );
  save("trace-breaker-aberto", open, "Requisicao curto-circuitada: nao existe span HTTP de saida para a parceira com o circuito aberto.");
  save("trace-servido-de-cache", cached, "Cotacao servida de cache: nenhum span HTTP de saida para as parceiras.");

  const forbidden = /document|cpf|plate|placa|quote_id/i;
  const leaks = new Set();
  for (const t of traces) for (const s of t.spans) for (const g of s.tags) if (forbidden.test(g.key)) leaks.add(g.key);
  writeFileSync(
    join(outDir, "atributos-de-span.txt"),
    `Chaves de atributo vistas em ${traces.length} traces de POST /quotes (Jaeger /api/traces):\n` +
      [...new Set(traces.flatMap((t) => t.spans.flatMap((s) => s.tags.map((g) => g.key))))].sort().join("\n") +
      `\n\nChaves com document|cpf|plate|placa|quote_id: ${leaks.size === 0 ? "nenhuma" : [...leaks].join(", ")}\n`,
  );
}

async function metricNames() {
  const names = (await getJSON(`${PROMETHEUS}/api/v1/label/__name__/values`)).data;
  const labels = {};
  for (const name of names.filter((n) => n.startsWith("quotation_"))) {
    const series = (await getJSON(`${PROMETHEUS}/api/v1/series?match[]=${encodeURIComponent(name)}`)).data;
    labels[name] = [...new Set(series.flatMap((s) => Object.keys(s)))].filter((k) => k !== "__name__").sort();
  }
  writeFileSync(
    join(outDir, "metricas-no-prometheus.txt"),
    `Nomes de metrica no Prometheus (/api/v1/label/__name__/values), sem as do proprio Collector:\n` +
      names.filter((n) => !n.startsWith("otelcol_") && !n.startsWith("scrape_") && n !== "up").join("\n") +
      `\n\nRotulos das metricas quotation_*:\n` +
      Object.entries(labels)
        .map(([n, l]) => `${n}: ${l.join(", ")}`)
        .join("\n") +
      "\n",
  );
}

for (const item of items) {
  if (charts[item]) await chart(item);
  else if (item === "traces-antes") await tracesBefore();
  else if (item === "traces-depois") await tracesAfter();
  else if (item === "metric-names") await metricNames();
  else throw new Error(`unknown item ${item}`);
}
