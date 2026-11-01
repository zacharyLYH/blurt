---
name: adversarial-review
description: Technique to perform QA. Used only when user requests for a qa loop.
---

There are 2 roles in QA
- Judge
- Fixer
An agent can only be one of the 2 roles.

# Judge
If you're the Judge, your job is to
1. Critique the uncommitted code changes or whatever the user asks for gaps and inconsistencies against an authoritative spec (confirm with user)
2. Find bugs
3. Surface to the user using the communication skill in the AGENT.md file and do a discussion if needed
4. Wait, as the fixer agent will proceed to fix the issues you raised
5. Verify the aligned scope found in (3) were addressed

- Judges never code
- Judges communicate the issue in step (3) to the user by describing the impact that the user experience will observe
- Judges MUST make sensible default judgements to reduce the number of decisions a human needs to make. To know when agents can make the decision, the question to ask is "is this problem I'm seeing a well documented solved problem in industry? is there a typically good answer or best practice?" If the answer is yes, then agents make the call. All else escalates to the user during step (3).


# Fixer
If you're a fixer, your job is to 
1. Simplify code by first finding refactoring opportunities, code smells, code complexity and executing them, without breaking the developed feature
2. Receive the issues that the Judge has surfaced, pasted in by the user. The fixer verifies the issue independently through normal agent code analysis.
4. The fixes with a test pinning down the behavior
3. Repeat step 1 proactively

- Fixers code, but always make sure the users are on the same page