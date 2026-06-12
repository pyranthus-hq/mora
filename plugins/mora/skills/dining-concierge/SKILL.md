---
name: dining-concierge
description: Use when recommending a restaurant, bar, cafe, happy hour, or any food/drink outing for the user — "where should I/we eat", date night, group dinner, birthday meal, coffee plans — especially when the request names people the user knows (e.g. "dinner with Sam", "lunch with Alex").
---

# Dining Concierge

Requires the Mora MCP server (`claude mcp add mora -s user -- mora mcp serve`). If the `mora` tools are not available, say so up front and proceed web-only — the result lacks personal grounding; never fabricate it.

## Overview

Restaurant picking is two layers: **live web** (what's open, deals, transit) and **personal context** (who's coming, your shared history, the occasion). The web layer is commodity; the personal layer is what makes the rec right — and it lives in Mora. **Query Mora BEFORE any web search.** A cold web-only rec is the documented failure mode this skill exists to prevent.

## The Flow

1. **Anchor time/place.** Get the current local time (`date`) — deal windows and "tonight" depend on it. Note the user's transit constraint if given.

2. **Mora layer first** (parallel `search_memory` calls):
   - Per person named: `"<name>"` → identity, solo + group threads, in-flight plans. **Group chats carry occasions** (birthdays, "dinner Tuesday 7pm") — read them.
   - Shared history: `"<name> dinner restaurant reservation"` → places they have actually been together.
   - Taste tier: `"Resy OpenTable reservation confirmed"` (adapt to local booking platforms) → the user's real venue history (price tier, cuisines).
   - Calendar: availability and next obligation around the proposed time.

3. **Web layer second.** Current deals/hours verified for TODAY, transit route, ride price, reservation availability. Apply regional gotchas (e.g. Massachusetts bans time-window alcohol discounts — only FOOD deals like $1 oysters exist there).
   - **Egress boundary:** web/map/ride queries carry only coarse public terms — city or neighborhood, cuisine, candidate venue names. Never put people's names, message text, phone numbers, or calendar event details into an external query. The personal layer informs *which* query to run, not its contents.

4. **Synthesize 2–3 ranked picks.** Each: why it fits (cite the retrieved evidence), logistics from the user's actual location/next obligation, reservation link, one fallback. End with an explicit **"what Mora does NOT know"** line (e.g. dietary needs never mentioned in threads → suggest asking).
   - **If Mora reveals an existing reservation or in-flight plan overlapping the window, surface it as pick #1** — never compete with the user's own plan; re-rank everything else as before/after/fallback options around it.

5. **Close the loop.** After the user picks, offer `write_memory`: venue, who, occasion, verdict. Future recs get sharper.

## Evidence Rules

- **Attendance ≠ preference.** Say "you've been there twice," never "you love it." Reservations prove presence, not satisfaction.
- **Cite only retrieved memories.** No evidence of someone's tastes → say so plainly. Never invent "X loves oysters."
- **Occasion sets altitude.** A birthday thread upgrades the venue tier; don't pitch a dive-bar happy hour for a celebration without flagging the mismatch.

## Privacy

- **Task-minimal retrieval.** Pull only what the recommendation needs; summarize evidence rather than quoting whole private threads, and never include phone numbers in output.
- Output still contains real names and history — never paste it into public demos, posts, or screenshots.
- Vault-derived details never leave the machine: see the egress boundary in step 3.

## Common Mistakes

| Mistake | Fix |
|---|---|
| Web search first, Mora after (or never) | Mora layer is step 2, always before web |
| Recommending for "tonight" without checking the clock | Anchor current local time first |
| Trusting listed hours/deals | Verify for today; deals rotate |
| Inferring group "compatibility" scores | Show evidence per person; no invented scalars |
| Treating a named person as unknown | Search Mora; the vault usually knows them |
