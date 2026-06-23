#!/usr/bin/env node
// Parallel drop-in for judge.mjs: identical grader prompt + model + output path,
// only the loop is concurrent (cap CONC). Skips cells that already have a
// .judge.json so it resumes whatever the serial judge already produced.
import { readFileSync, writeFileSync, readdirSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { execFile } from 'node:child_process';

const [outDir, qfile] = process.argv.slice(2);
const JUDGE_MODEL = 'haiku';
const CONC = Number(process.env.CONC || 8);
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

const sys = 'You are a STRICT grader. Compare a candidate answer to a gold reference. Output ONLY a JSON object (no prose, no markdown fences): {"accuracy": <0-100 how much of the gold answer\'s key facts the candidate got right>, "found_key_facts": <true if it surfaced the single most important point, esp. any blocker/dropped commitment>, "fabricated": <true if it asserts facts NOT in the gold reference>, "notes": "<one sentence>"}.';

function grade(question, gold, candidate) {
  return new Promise((resolve) => {
    const prompt = `QUESTION:\n${question}\n\nGOLD REFERENCE ANSWER:\n${gold}\n\nCANDIDATE ANSWER:\n${candidate || '(empty)'}\n\nGrade now. JSON only.`;
    execFile('claude', ['-p', prompt, '--model', JUDGE_MODEL, '--output-format', 'json',
      '--strict-mcp-config', '--mcp-config', join(outDir, '..', 'baseline-mcp.json'),
      '--append-system-prompt', sys, '--dangerously-skip-permissions'],
      { encoding: 'utf8', maxBuffer: 1 << 24 }, (e, out) => {
        if (e) return resolve({ accuracy: 0, found_key_facts: false, fabricated: false, notes: 'judge call failed: ' + String(e).slice(0, 80) });
        let res = ''; try { res = JSON.parse(out).result ?? ''; } catch { res = out; }
        const m = res.match(/\{[\s\S]*\}/);
        try { resolve(JSON.parse(m ? m[0] : res)); }
        catch { resolve({ accuracy: 0, found_key_facts: false, fabricated: false, notes: 'unparseable judge output' }); }
      });
  });
}

const todo = readdirSync(outDir)
  .filter(f => /\.(baseline|mora-mcp-mmr|mora-mcp|mora-cli)\.\d+\.jsonl$/.test(f))
  .filter(f => !existsSync(join(outDir, f.replace('.jsonl', '.judge.json'))));
process.stderr.write(`parallel judge: ${todo.length} cells, CONC=${CONC}\n`);

let i = 0, done = 0;
async function worker() {
  while (i < todo.length) {
    const f = todo[i++];
    const qid = f.split('.')[0];
    const gold = GOLD.get(qid); if (!gold) continue;
    const { txt, err } = finalAnswer(join(outDir, f));
    const verdict = err ? { accuracy: 0, found_key_facts: false, fabricated: false, notes: 'arm errored' }
                        : await grade(gold.question, gold.reference_answer, txt);
    writeFileSync(join(outDir, f.replace('.jsonl', '.judge.json')), JSON.stringify(verdict, null, 1));
    process.stderr.write(`judged ${f}: acc=${verdict.accuracy} key=${verdict.found_key_facts} fab=${verdict.fabricated}  [${++done}/${todo.length}]\n`);
  }
}
await Promise.all(Array.from({ length: Math.min(CONC, todo.length) }, worker));
process.stderr.write('parallel judge done\n');
