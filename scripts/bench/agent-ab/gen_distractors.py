#!/usr/bin/env python3
"""Generate plausible DISTRACTOR memories to bury the gold answers in noise.

Usage: gen_distractors.py <world.json> <out world-large.json> [N=1900]

Produces world-large.json = the original gold memories + N programmatically
generated distractors that reuse the SAME people / projects / sources / date
range so brute-force grep gets distracted and semantic retrieval must
discriminate. Distractors deliberately collide on gold keywords ("offer",
"remote", "pickup", "Saturday", "Marcus", "Ridgeline", "procurement") in BENIGN
contexts, but NEVER contain a gold fact (no Veridian/relocation, no UA1492/SFO
4:35, no Ridgeline-Saturday-4pm/term-sheet, no mvance@ alias, no SOC2 blocker).
Zero API cost — pure templates. Deterministic (seeded)."""
import json, random, sys

random.seed(1337)

PEOPLE = ["Jordan Kim", "Priya Nair", "Roberto Rivera", "Lena Brooks",
          "Marcus Vance", "Omar Haddad", "Dana Liu", "Marcus Chen", "Tariq Osei",
          "Nina Park", "Greg Holt", "Sofia Marin"]
WORK_PEOPLE = ["Priya Nair", "Dana Liu", "Tariq Osei", "Nina Park", "Greg Holt"]
PROJECTS = ["Halcyon", "the dashboard rewrite", "the billing migration",
            "the onboarding flow", "the SDK", "the status page", "infra cleanup",
            "the design system", "the data pipeline", "the mobile build"]
CUSTOMERS = ["Northwind", "Brightfield", "Cedarworks", "Pineapp", "Lumen Logistics",
             "Quanta Retail", "Hollowbrook", "Meridian Foods"]
VENDORS = [("Stripe <receipts@stripe.com>", "Stripe"), ("Amazon.com <auto-confirm@amazon.com>", "Amazon"),
           ("GitHub <noreply@github.com>", "GitHub"), ("DoorDash <no-reply@doordash.com>", "DoorDash"),
           ("Vercel <ship@vercel.com>", "Vercel"), ("Linear <notifications@linear.app>", "Linear"),
           ("Datadog <alerts@datadoghq.com>", "Datadog"), ("TLDR Newsletter <dan@tldrnewsletter.com>", "TLDR")]
PHONES = {"Jordan Kim": "+1-415-555-0188", "Priya Nair": "+1-415-555-0142",
          "Roberto Rivera": "+1-510-555-0177", "Lena Brooks": "+1-628-555-0199",
          "Dana Liu": "+1-415-555-0211", "Marcus Chen": "+1-650-555-0173",
          "Tariq Osei": "+1-628-555-0150", "Nina Park": "+1-415-555-0190"}

def rdate():
    # span wider than the gold range (May-Jun) to add temporal noise
    m = random.choice([3, 3, 4, 4, 5, 5, 6])
    d = random.randint(1, 27 if m == 6 else 28)
    return f"2026-{m:02d}-{d:02d}"

# --- per-source template banks: (title, body). {p}=person {proj}=project {cust}=customer {n}=number
WORK_MAIL = [
    ("Standup notes — {proj}", "Quick recap from standup. {p} is unblocked on {proj}; CI is green again after the flaky test fix. No customer impact. Nothing for you to action."),
    ("PR #{n} ready for review", "Opened PR #{n} on {proj} — small refactor, {n} files. Tests pass. Can you take a look when you get a sec? Not urgent."),
    ("Re: weekly metrics", "Numbers for the week: signups up {n}%, churn flat. {cust} usage steady. Full dashboard in the shared sheet. Talk at the sync."),
    ("Infra alert (resolved)", "Datadog flagged elevated latency on the {proj} service for ~{n} min, then auto-recovered. Root cause was a slow query, already patched. No action needed."),
    ("Re: {cust} check-in recap", "Good call with {cust} today — they're happy with the rollout, asked about SSO timeline. I said Q3. Logged the notes in Linear. No blockers."),
    ("Design review for {proj}", "{p} walked us through the new {proj} mocks. Looks clean. One nit on spacing. Shipping behind a flag next week."),
    ("Re: vendor renewal", "Our {proj} tooling renewal is up next month, ${n}00/mo. Same plan. I'll just auto-renew unless you object."),
    ("Recruiting: {p} intro", "{p} reached out about the platform role — strong resume, {n} years infra. Want me to set up a first call? No rush, pipeline is healthy."),
    ("Re: roadmap nits", "Tightened the roadmap doc for {proj}. Moved two items to next cycle. Nothing controversial. Will present at all-hands."),
    ("Support ticket #{n} closed", "Closed the {cust} support ticket — was a config issue on their end. Resolved in {n} min. They're all set."),
    ("Offer: annual plan discount", "Heads up, {p}'s team is offering {n}% off the annual tier through end of month. Probably worth it for us. Your call."),
    ("Re: remote week poll", "We're polling the team on a remote-only week next month vs an in-office sprint. {p} voted remote. Add your vote in the thread."),
    ("Procurement form for {cust}", "Routine procurement paperwork for the {cust} renewal — standard MSA, nothing unusual. Legal already glanced at it. Sign when you can."),
]
PERSONAL_MAIL = [
    ("Your {v} receipt", "Thanks for your order. Total ${n}.{n}. This is an automated receipt; no reply needed."),
    ("{v}: your weekly digest", "Top stories this week plus a few deals. Unsubscribe anytime. — {v}"),
    ("Re: dinner sometime?", "Was great seeing you! Let's grab food soon — maybe a weeknight. I'm flexible. No pressure on timing."),
    ("Flight deal alert", "Fares to {n} cities dropped this week. Round trips from ${n}9. Not a booking, just a heads-up from your fare tracker."),
    ("Gym membership update", "Your plan renews next month at ${n}9/mo. New classes added. Manage your membership in the app."),
    ("Package delivered", "Your {v} package was delivered to the front door. {n} item(s). Tap for a photo."),
    ("Re: weekend hike?", "Down for a hike if the weather holds. Not this Saturday though — maybe the one after. Lmk what works."),
    ("Card statement ready", "Your statement is ready. Balance ${n}{n}. Autopay is on. Nothing due manually."),
]
IMSG = [
    ("iMessage: {p} — lunch?", "From: {p} ({ph})\n\n{p}: lunch today? thinking the taco place\nSam: maybe, slammed til 2\n{p}: no worries, rain check"),
    ("iMessage: {p} — gym", "From: {p} ({ph})\n\n{p}: gym at 7?\nSam: can't, call runs late\n{p}: tmrw then"),
    ("iMessage: {p} — random link", "From: {p} ({ph})\n\n{p}: lol you have to see this\n{p}: [link]\nSam: hah amazing"),
    ("iMessage: {p} — weekend plans", "From: {p} ({ph})\n\n{p}: doing anything fun this weekend?\nSam: probably catching up on work tbh\n{p}: classic"),
    ("iMessage: {p} — coffee", "From: {p} ({ph})\n\n{p}: coffee before standup?\nSam: yes pls\n{p}: usual spot 8:45"),
    ("iMessage: {p} — pickup", "From: {p} ({ph})\n\n{p}: can you grab the grocery pickup on the way? it's already paid\nSam: sure, order number?\n{p}: texted it to you"),
    ("iMessage: {p} — remote day", "From: {p} ({ph})\n\n{p}: wfh tomorrow? heads down on the SDK\nSam: same, remote day for me too\n{p}: 👍"),
    ("iMessage: {p} — offer ended", "From: {p} ({ph})\n\n{p}: that pizza offer you sent expired btw\nSam: dang. next time\n{p}: ha"),
    ("iMessage: {p} — happy birthday", "From: {p} ({ph})\n\n{p}: happy bday!! 🎉\nSam: thank you!!\n{p}: we should celebrate"),
    ("iMessage: {p} — quick q", "From: {p} ({ph})\n\n{p}: did the deploy go out?\nSam: yep all green\n{p}: nice"),
]
CAL = [
    ("Standup — {proj}", "Daily standup. {p} hosting. 15 min. Recurring."),
    ("1:1 with {p}", "Weekly 1:1. Usual agenda doc. 30 min."),
    ("Lunch with {p}", "Casual lunch. {p}'s pick. No agenda."),
    ("Dentist", "Routine cleaning. Bring insurance card."),
    ("{cust} sync", "Status sync with {cust}. Nothing major on the agenda."),
    ("Gym", "Strength session. 45 min."),
    ("Design crit — {proj}", "Review latest {proj} mocks with {p}."),
    ("Investor intro — {p}", "Intro coffee with {p}, angel. Exploratory only, no deck needed."),
    ("Haircut", "Usual barber. 30 min."),
    ("Sprint planning", "Plan next sprint for {proj}. {p} facilitating."),
]
NOTE = [
    ("Grocery list", "eggs, oats, coffee, spinach, chicken, olive oil, that hot sauce {p} likes"),
    ("Ideas — {proj}", "random thoughts on {proj}: maybe cache the {n} endpoint, revisit the empty state, ask {p} about the edge case"),
    ("Books to read", "started the one {p} recommended. queue: {n} more on the list. the founder bio was mid."),
    ("TODO scratch", "- reply to {p}\n- renew domain\n- expense the {v} receipt\n- water the plants"),
    ("Workout log", "{n} min run, felt ok. legs tomorrow. {p} says try the new class."),
    ("Meeting doodles", "notes from the {cust} call — nothing urgent, they're happy. follow up next month maybe."),
    ("Gift ideas", "for {p}: that gadget, a book, concert tickets? budget ~${n}0"),
    ("Random", "remember to ask {p} about the {proj} thing. also: is the offer site still up? check later."),
]

def fill(t):
    return (t.replace("{p}", random.choice(PEOPLE))
             .replace("{proj}", random.choice(PROJECTS))
             .replace("{cust}", random.choice(CUSTOMERS))
             .replace("{n}", str(random.randint(2, 89)))
             .replace("{v}", random.choice(VENDORS)[1]))

def make(source):
    date = rdate()
    if source == "gmail":
        work = random.random() < 0.6
        if work and random.random() < 0.2:  # vendor/automated into work box
            frm = random.choice(VENDORS)[0]; title, body = random.choice(PERSONAL_MAIL)
        elif work:
            frm = random.choice(WORK_PEOPLE); title, body = random.choice(WORK_MAIL)
        else:
            frm = random.choice(VENDORS)[0]; title, body = random.choice(PERSONAL_MAIL)
        title, body = fill(title), fill(body)
        mailbox = "sam@halcyon.dev" if work else "sam.rivera.personal@gmail.com"
        scope = "project:halcyon" if work else "personal"
        return {"source": "gmail", "from": frm, "participants": [frm, "Sam Rivera <sam@halcyon.dev>"],
                "date": date, "mailbox": mailbox, "scope": scope, "title": title,
                "body": f"From: {frm}\nSubject: {title}\n\n{body}"}
    if source == "imessage":
        p = random.choice([x for x in PEOPLE if x in PHONES])
        title, body = random.choice(IMSG)
        ph = PHONES[p]
        title = title.replace("{p}", p); body = body.replace("{p}", p).replace("{ph}", ph)
        title, body = fill(title), fill(body)
        scope = "personal" if p in ("Roberto Rivera", "Jordan Kim", "Marcus Chen") else random.choice(["personal", "project:halcyon"])
        return {"source": "imessage", "from": p, "participants": [f"{p} ({ph})", "Sam Rivera"],
                "date": date, "scope": scope, "title": title, "body": body}
    if source == "calendar":
        title, body = random.choice(CAL); title, body = fill(title), fill(body)
        scope = random.choice(["project:halcyon", "personal"])
        return {"source": "calendar", "from": "Sam Rivera", "participants": ["Sam Rivera"],
                "date": date, "scope": scope, "title": title, "body": body}
    title, body = random.choice(NOTE); title, body = fill(title), fill(body)
    return {"source": "note", "from": "Sam Rivera", "participants": ["Sam Rivera"],
            "date": date, "scope": random.choice(["personal", "project:halcyon"]),
            "title": title, "body": body}

def main():
    world_path, out_path = sys.argv[1], sys.argv[2]
    N = int(sys.argv[3]) if len(sys.argv) > 3 else 1900
    world = json.load(open(world_path))
    gold = world["memories"]
    # match the gold source mix so noise looks native
    weights = {"gmail": 0.38, "imessage": 0.32, "calendar": 0.16, "note": 0.14}
    srcs, ws = list(weights), list(weights.values())
    distractors = [make(random.choices(srcs, ws)[0]) for _ in range(N)]
    out = {"memories": gold + distractors, "questions": world.get("questions", [])}
    json.dump(out, open(out_path, "w"), indent=1)
    from collections import Counter
    mix = Counter(m["source"] for m in distractors)
    print(f"wrote {out_path}: {len(gold)} gold + {N} distractors = {len(out['memories'])} memories")
    print(f"distractor source mix: {dict(mix)}")

if __name__ == "__main__":
    main()
