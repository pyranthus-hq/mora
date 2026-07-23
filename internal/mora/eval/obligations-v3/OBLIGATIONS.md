# Obligation contract, version 3

This contract is the complete adjudication rule for the obligations-v3 corpus. Read the record as of the corpus timestamp and use only text and source metadata visible in that record. An obligation is a concrete future action that one person has accepted, promised, requested, or assigned for another person. The action, owner, and beneficiary or requester must be identifiable. Polite wording, questions, and statements can all create obligations; greetings, facts, plans without an accountable action, and vague hopes do not.

An obligation stays open until visible evidence says it was delivered, completed, accepted as cancelled, or replaced. Silence, age, a forwarded copy, or a quoted copy does not close it. A quoted earlier ask is dead only when the fixture itself shows the fulfillment. If the authored reply says the requested item was sent and the earlier ask appears beneath it as a quote, the ask is closed; a quote by itself is not proof of closure. When a thread contains both the request and the reply that completes it, the obligation is closed in that thread.

Copies do not create extra work. A note, forward, or quote that repeats the same action for the same owner and counterparty is evidence of the original obligation, not a second current obligation. A materially changed action, owner, beneficiary, or deadline is a separate obligation.

Source structure matters. Authored text is read as the sender's text. In Gmail, use each message's sender and addressees for that message; the thread-level From, To, and Cc lists are only a union and do not determine who owes a particular action. Attributed quoted and forwarded blocks retain the attribution shown by the record. Footers, signatures, legal notices, unsubscribe language, bare links, automated notices, and advertisements do not create personal obligations merely because they use imperative language. A bare forwarded advertisement is not an ask. If the forwarder adds a personal line asking the user to do a concrete action, that authored line can create an obligation; the advertisement still does not.

A calendar title or agenda item describes what a meeting will cover. It is not a personal assignment unless the text actually assigns or requests an action from a named or otherwise unambiguous person. Attendance alone does not make every agenda item the user's task.

## things the user must do

Put an open obligation in **things the user must do** (`owed_by_self`) when the user is the person who owes the concrete action. This includes a request directed to the user and the user's own clear promise to another person. The sender is not automatically the owner: in “Could you send the draft?”, the recipient owes the action; in “I’ll send the draft,” the sender owes it.

Do not put completed, cancelled, replaced, or duplicate copies in this section. Do not put an action owed between two other people here merely because the user was copied.

## waiting on others

Put an open obligation in **waiting on others** (`owed_by_counterparty`) when another person owes the concrete action to the user. A direct promise such as “I will send you the cost grid” belongs here. A request written by the user belongs here only when the other person accepted it or the request itself clearly assigns that person the action.

Do not put the user's own work in this section. Do not surface work owed solely between other people, even if it is a real assignment visible in a copied message.

## due time and surfaces

Record an explicit due time only when the text supplies a calendar date or timestamp. Keep phrases such as “tomorrow,” “before the review,” and “by the end of the week” as relative due language. Do not invent a deadline from urgency, recency, or meeting placement.

The **DAILY** surface follows one uniform rule: include an obligation if and only if it is open, canonical (not a duplicate copy), belongs in one of the two user-facing sections above, and its opening message timestamp falls within the trailing seven 24-hour periods ending at the corpus `as_of` timestamp, inclusive at both endpoints. Closed, replaced, duplicate, third-party-only, and non-obligation records never qualify. There is no manual promotion based on importance or channel.

A named meeting surface includes an otherwise-current obligation only when its owner or counterparty is an attendee and the obligation text explicitly names, or unmistakably refers to, that meeting. Merely occurring in email with an attendee is not enough. Surface placement never changes owner, direction, lifecycle, or due time.
