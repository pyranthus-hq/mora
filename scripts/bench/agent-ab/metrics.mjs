#!/usr/bin/env node
// Aggregate the A/B/C transcripts: per arm, the median cost / billable-tokens /
// turns / tool-calls / exploratory-reads, plus LLM-judge accuracy if present.
import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { join } from 'node:path';

const [outDir, qfile] = process.argv.slice(2);
const ARMS = ['baseline', 'mora-mcp', 'mora-cli'];
const READ_TOOLS = new Set(['Read', 'Grep', 'Glob', 'LS']);

function parseCell(path) {
  const tools = {};
  let cost = 0, inTok = 0, cacheC = 0, cacheR = 0, outTok = 0, turns = 0, err = false;
  for (const ln of readFileSync(path, 'utf8').split('\n')) {
    if (!ln.trim()) continue;
    let e; try { e = JSON.parse(ln); } catch { continue; }
    if (e.type === 'assistant' && e.message?.content)
      for (const b of e.message.content) if (b.type === 'tool_use') tools[b.name] = (tools[b.name] || 0) + 1;
    if (e.type === 'result') {
      cost = e.total_cost_usd ?? cost; turns = e.num_turns ?? turns; err = !!e.is_error;
      const u = e.usage || {};
      inTok = u.input_tokens || 0; outTok = u.output_tokens || 0;
      cacheC = u.cache_creation_input_tokens || 0; cacheR = u.cache_read_input_tokens || 0;
    }
  }
  const toolCalls = Object.values(tools).reduce((a, b) => a + b, 0);
  const reads = Object.entries(tools).filter(([k]) => READ_TOOLS.has(k)).reduce((a, [, v]) => a + v, 0);
  const moraCalls = Object.entries(tools).filter(([k]) => k.toLowerCase().includes('mora')).reduce((a, [, v]) => a + v, 0);
  return { cost, billable: inTok + cacheC, cacheR, outTok, turns, toolCalls, reads, moraCalls, err };
}
const judge = (p) => existsSync(p) ? JSON.parse(readFileSync(p, 'utf8')) : null;

const med = (a) => { if (!a.length) return 0; const s = [...a].sort((x, y) => x - y); const m = s.length >> 1; return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2; };
const mean = (a) => a.length ? a.reduce((x, y) => x + y, 0) / a.length : 0;
const rng = (a) => a.length ? `${Math.min(...a)}–${Math.max(...a)}` : '–';
const usd = (n) => '$' + n.toFixed(3);

// cells[qid][arm] = [{cell, judge}]
const cells = {};
for (const f of readdirSync(outDir)) {
  const m = f.match(/^(.+)\.(baseline|mora-mcp|mora-cli)\.(\d+)\.jsonl$/);
  if (!m) continue;
  const [, qid, arm] = m;
  (cells[qid] ??= {})[arm] ??= [];
  cells[qid][arm].push({ c: parseCell(join(outDir, f)), j: judge(join(outDir, f.replace('.jsonl', '.judge.json'))) });
}

const qids = Object.keys(cells).sort();
console.log('\n=== PER-QUESTION (median of reps) ===');
console.log(['qid', 'arm', 'cost', 'billTok', 'turns', 'tools', 'reads', 'mora', 'acc', 'foundKey', 'errs'].join('\t'));
const agg = {}; ARMS.forEach(a => agg[a] = { cost: [], bill: [], turns: [], tools: [], reads: [], acc: [], key: [], err: 0, n: 0 });
for (const qid of qids) for (const arm of ARMS) {
  const cs = cells[qid]?.[arm]; if (!cs?.length) continue;
  const C = cs.map(x => x.c), J = cs.map(x => x.j).filter(Boolean);
  const acc = J.length ? mean(J.map(j => j.accuracy || 0)) : NaN;
  const key = J.length ? mean(J.map(j => j.found_key_facts ? 1 : 0)) : NaN;
  const errs = C.filter(c => c.err).length;
  console.log([qid, arm, usd(med(C.map(c => c.cost))), med(C.map(c => c.billable)), med(C.map(c => c.turns)),
    med(C.map(c => c.toolCalls)), med(C.map(c => c.reads)), med(C.map(c => c.moraCalls)),
    Number.isNaN(acc) ? '?' : acc.toFixed(0), Number.isNaN(key) ? '?' : key.toFixed(2), errs].join('\t'));
  const a = agg[arm];
  a.cost.push(med(C.map(c => c.cost))); a.bill.push(med(C.map(c => c.billable)));
  a.turns.push(med(C.map(c => c.turns))); a.tools.push(med(C.map(c => c.toolCalls)));
  a.reads.push(med(C.map(c => c.reads))); if (!Number.isNaN(acc)) a.acc.push(acc);
  if (!Number.isNaN(key)) a.key.push(key); a.err += errs; a.n += C.length;
}

console.log('\n=== AGGREGATE (median across questions) ===');
for (const arm of ARMS) {
  const a = agg[arm]; if (!a.n) continue;
  console.log(`${arm.padEnd(9)} cost=${usd(med(a.cost))}  billTok=${med(a.bill)} (${rng(a.bill)})  turns=${med(a.turns)}  tools=${med(a.tools)}  reads=${med(a.reads)}  acc=${a.acc.length ? mean(a.acc).toFixed(0) : '?'}  foundKey=${a.key.length ? (mean(a.key) * 100).toFixed(0) + '%' : '?'}  errs=${a.err}/${a.n}`);
}
const b = agg.baseline;
console.log('\n=== DELTAS vs baseline ===');
for (const arm of ['mora-mcp', 'mora-cli']) {
  const a = agg[arm]; if (!a.n || !b.n) continue;
  const cR = med(b.cost) ? (med(a.cost) / med(b.cost)) : 0;
  const accD = (a.acc.length && b.acc.length) ? (mean(a.acc) - mean(b.acc)).toFixed(0) : '?';
  console.log(`${arm}: cost ${usd(med(a.cost))} (${cR ? cR.toFixed(2) + '×' : '–'} of baseline)  accuracy ${a.acc.length ? mean(a.acc).toFixed(0) : '?'} vs ${b.acc.length ? mean(b.acc).toFixed(0) : '?'} (Δ${accD})`);
}
console.log('\nNOTE: acc = LLM-judged accuracy 0-100 vs synthetic gold; foundKey = % of reps that surfaced the critical fact (e.g. the iMessage-only dropped commitment). billTok = input+cache-creation (paid input). Hermetic synthetic vault; cost from Claude\'s own result event.\n');
