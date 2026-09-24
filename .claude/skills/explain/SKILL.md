---
name: explain
description: Explain one named point — a term, an issue or PR number, a decision, a line of code, a spec clause — briefly and in the context it belongs to, using that context's own vocabulary. Use when asked to explain, clarify or unpack something, or when a reply was understood in a way that was not meant.
user-invocable: true
---

# /explain — one point, in its own context

`/explain <point>` where the point is whatever needs unpacking: `reserveBulk`,
`#180`, "why the bound still has to be compared", a decision taken earlier in the
conversation, a paragraph of the spec.

The reader knows the project. They do **not** currently know this one thing, or they
know it in a way that does not fit. The job is to close that specific gap — not to
introduce the project, and not to restate the whole thread.

## Check before explaining

**The single most expensive failure here is explaining from memory.** In one session
a rule was explained confidently from code that had been superseded two days
earlier; the explanation was coherent, detailed, and wrong, and hours of work were
built on it.

So, before writing: if the point touches **code**, read the code. If it touches a
**spec**, open the spec and quote the clause. If it touches an **issue or PR**, fetch
it. If it touches something decided **earlier in this conversation**, scroll back
rather than reconstruct. If the source cannot be reached, say which part is from
recollection.

## Shape

Four moves, in this order. Skip any that the reader plainly already has.

1. **The setup** — what the thing sits inside, in two or three sentences. Usually
   the real gap: `reserveBulk` makes no sense until you know the encoder has a fast
   route and a slow one, and that the fast one cannot check as it writes.
2. **The point itself** — what it is, or what changed, or why it is that way.
3. **The evidence**, where there is any. A measured number, a real error string, the
   actual generated line. One concrete artefact beats three sentences:
   `bytesUsed: 11, buffer: 5` settled a question that prose had not.
4. **Where it stops** — what it does not cover, what it costs, which case it
   deliberately excludes. An explanation that only says what works leaves the reader
   to discover the edge on their own.

## Vocabulary

Use the context's own names, exactly as they are spelled there: `arrayBulk`,
`Uint16Array`, `MAX_SIZE`, §7.1, `LimitExceeded`, corelib-ts#177. A reader who
recognises the names can place the explanation immediately, and can grep for more.

Use plain words for the **mechanism** — "writes past the buffer", "grows as it
fills", "the container cannot hold anything wider". The names are precise; the
sentences around them do not have to be formal to be exact.

Do not invent a term the project does not use, and do not paraphrase one it does
into something friendlier. Renaming the thing is how an explanation stops being
about the thing.

## Length

Short. A few bullets or two or three small sections. If it will not fit, the point
was really several points — say which ones and offer them, rather than writing all
of them unasked.

A table earns its place when the point **is** a comparison (per element kind, before
and after, which cases are covered). Otherwise it is padding.

## What this is not

* Not a summary of the conversation — the reader was there.
* Not a status report. If they wanted to know where things stand, they asked
  something else.
* Not a defence of a decision. If the point is a decision, explain what it trades
  and what it rules out; if it turns out to be wrong, say so plainly and move on.
* Not a place for hedging. If something is uncertain, name the uncertainty and its
  cause in one clause, then continue.

Write the explanation in the language the conversation is using.
