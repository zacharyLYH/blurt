# How to talk to me
When it comes to communication, the goal is to help me get to understanding in the shortest time possible, down to the second. Here's how we'll achieve this goal:

Prose:
- Summary; The efficiency of Singlish. Most compact way to convey ideas through words.
- Pretend you're doing an elevator pitch. Short, punchy, direct sentences paried with strongly meaningful word choices are your north star goal when it comes to communication.
- Use bad grammar to optimize for succintness; user can easily fill in fragments. Keep verbs and nouns. But do not gloss over important points.
- Keep prose casual and non techncial word choice simple. This is to optimize for skimmability. No preamble. Technical jargon is allowed but assume user needs a bit of help with uncommon terms.


Talking points and structure:
- Summary; High level to low level
- In sequence, make sure the product is aligned, user journey is established, high level implementation idea is aligned, implementation detail is aligned. Do not jump into a deep step until me brain is one with your idea. You have the vested power to even waste a turn confirming things!
- Taxonomy lock. Once we have chosen an apt name to represent a concept or entity, then stick to that forever. This is to drive adoption and consistency. No synonym drift.
- Periodically, tie back the conversation to the user's current mental model. This is helpful to let user see the bigger picture.
- Point out pros, cons, and edge cases where applicable to let user develop better mental model for the choices they need to make

# Coding standards
1. MINIMAL DEPENDENCIES. Fewer dependencies and the more mainstream and battle tested the dependencies, the better. 
2. MINIMAL LINES OF CODE. More lines of code === more complexity. That is evil.
3. JUDGEMENT CALLS SHOULD BE SENSIBLE DEFAULTS. Sometimes users don't spell out every minute detail, sensible defaults is your target when behavior / features are ambiguous. One thing you can ask yourself is "How would Github Codespaces a polished Saas app do this behavior if they had to?"
4. DEFER TESTS UNTIL USER HAS CONFIRMED FEATURE PARITY. Otherwise, you might just be wasting user's time creating tests when the feature might not be ready yet!
5. NO THIN MODULES NOR BIG FUNCTIONS. To know when abstraction is a good idea, ask yourself if reading some abstracted function actually reduces mental friction. 
6. ALWAYS USE UNSLOP SKILL!

# Go code
1. INTERFACES AND MOCKS. APIs should be interfaces so that they can be easily mocked using Mockery.
2. TESTS. Unit tests and intg tests are a must. Where it makes sense, use the mockery generated mocks.

# Typescript code
1. TYPED. Everything must be typed. 
2. TESTS. Playwright behavioral tests should be first class from day 1 (visual/screenshot tests deferred for now). Mocks are allowed, but push it down as far as possible - we want to exercise as much behavior as possible!
3. USE SHADCN COMPONENTS AND TAILWIND CSS. Prioritize using fleshed out and polished ShadCN components found here: https://ui.shadcn.com/docs/components
4. TAILWIND CSS RESPONSIVE DESIGN IS MOBILE FIRST. Custom styles are for larger screens, non-custom are assumed to be for mobile screens.
5. MOCK API BEHAVIOR. Do not spin up a real golang server. Just mock the API behavior your test expects! Keeps code decoupled and even allows us to test API failure / edge cases much easier!

# Agent learnings
Anytime you have a tough time doing something and learn of a thing that can help future agents expedite this process, you should document it as a skill. These are some examples of things that should be skills:
1. Sharp edge that requires workarounds 
2. Weird setup requirements for testing 
3. Custom throwaway code you wrote that helps us do debugging in general - not some script that helps us debug only one edge case

Write it into the skills folder. This is how to structure skills inside this folder: 
```
my-skill/
├── SKILL.md              # Required: main instructions
├── references/           # Optional: additional documentation
│   ├── api-guide.md
│   ├── examples.md
│   └── troubleshooting.md
├── scripts/              # Optional: executable code
│   ├── generate.py
│   └── validate.sh
├── templates/            # Optional: file templates
│   ├── component.tsx.template
│   └── test.spec.ts.template
└── assets/               # Optional: static files
    ├── logo.png
    └── config.json
```
```
SKILL.md (Required)
The main skill file containing:

YAML frontmatter with metadata
Markdown instructions for Claude
---
name: my-skill
description: What this skill does and when to use it.
---

# Skill Title

Instructions go here...
```

What DOESN'T go in here are:
1. Details that can be found or easily inferred in code
2. Information about the business