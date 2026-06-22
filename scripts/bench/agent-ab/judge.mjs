#!/usr/bin/env node
// LLM judge: score each cell's final answer against the synthetic gold answer.
// Writes out/<qid>.<arm>.<rep>.judge.json = {accuracy,found_key_facts,fabricated,notes}.
// Uses a cheap fixed judge model (haiku) so grading cost stays low + independent
// of the arm's answer model.
import { readFileSync, writeFileSync, readdirSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';

const [outDir, qfile, _model] = process.argv.slice(2);
const JUDGE_MODEL = 'haiku';
const GOLD = new Map();
for (const q of JSON.parse(readFileSync(qfile, 'utf8'))) GOLD.set(q.id, q);

function finalAnswer(path) {
  let txt = '', err = false;
  for (const ln of readFileSync(path, 'utf8').split('\n')) {
    if (!ln.trim()) continue;
    let e; try { e = JSON.parse(ln); } catch { continue; }
    if (e.type === 'result') { txt = e.result ?? ''; err = !!e.is_error; }
  }
  return { txt, err };
}

function grade(question, gold, candidate) {
  const sys = 'You are a STRICT grader. Compare a candidate answer to a gold reference. Output ONLY a JSON object (no prose, no markdown fences): {"accuracy": <0-100 how much of the gold answer\'s key facts the candidate got right>, "found_key_facts": <true if it surfaced the single most important point, esp. any blocker/dropped commitment>, "fabricated": <true if it asserts facts NOT in the gold reference>, "notes": "<one sentence>"}.';
  const prompt = `QUESTION:\n${question}\n\nGOLD REFERENCE ANSWER:\n${gold}\n\nCANDIDATE ANSWER:\n${candidate || '(empty)'}\n\nGrade now. JSON only.`;
  let out;
  try {
    out = execFileSync('claude', ['-p', prompt, '--model', JUDGE_MODEL, '--output-format', 'json',
      '--strict-mcp-config', '--mcp-config', join(outDir, '..', 'baseline-mcp.json'),
      '--append-system-prompt', sys, '--dangerously-skip-permissions'],
      { encoding: 'utf8', maxBuffer: 1 << 24 });
  } catch (e) { return { accuracy: 0, found_key_facts: false, fabricated: false, notes: 'judge call failed: ' + String(e).slice(0, 80) }; }
  let res = '';
  try { res = JSON.parse(out).result ?? ''; } catch { res = out; }
  const m = res.match(/\{[\s\S]*\}/);
  try { return JSON.parse(m ? m[0] : res); }
  catch { return { accuracy: 0, found_key_facts: false, fabricated: false, notes: 'unparseable judge output' }; }
}

const cells = readdirSync(outDir).filter(f => /\.(baseline|mora-mcp|mora-cli)\.\d+\.jsonl$/.test(f));
for (const f of cells) {
  const jpath = join(outDir, f.replace('.jsonl', '.judge.json'));
  if (existsSync(jpath)) continue;
  const qid = f.split('.')[0];
  const gold = GOLD.get(qid);
  if (!gold) continue;
  const { txt, err } = finalAnswer(join(outDir, f));
  const verdict = err ? { accuracy: 0, found_key_facts: false, fabricated: false, notes: 'arm errored' }
                      : grade(gold.question, gold.reference_answer, txt);
  writeFileSync(jpath, JSON.stringify(verdict, null, 1));
  process.stderr.write(`judged ${f}: acc=${verdict.accuracy} key=${verdict.found_key_facts} fab=${verdict.fabricated}\n`);
}
