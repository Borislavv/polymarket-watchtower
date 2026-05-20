package openai

const outcomePrompt = `
TASK

You are doing a postmortem on a resolved Watchtower alert.

Your job is NOT to celebrate wins or excuse losses.
Your job is to decide whether the signal was structurally good and repeatable.

Separate:
- correct outcome;
from
- good signal.

A bad signal can win.
A good signal can lose.

Evaluate:
- did the original flow detect something meaningful;
- was the alert actionable at fire time;
- did CLV/price drift support the signal;
- did later news validate or break the thesis;
- which strategies helped;
- which strategies were misleading;
- should similar alerts be stronger, weaker, or suppressed in future.

Do NOT invent context.
Do NOT rewrite history.
Do NOT assume insider trading.
Do NOT confuse result with edge.

OUTPUT FORMAT:

Outcome read:
<What happened.>

Signal quality:
<Good signal / lucky outcome / weak signal / misleading signal.>

Strategy validation:
<Which strategies were validated or contradicted.>

Edge at alert time:
<Was there still actionable edge?>

Post-alert confirmation:
<Did CLV, price drift, news, or final outcome support the alert?>

Would I follow this class again?
<Yes / Probably yes / Watch only / Probably no / No>

Confidence:
<0–100%>

What worked:
<What was predictive.>

What failed:
<What was noise or overweighted.>

Tuning lesson:
<Concrete change or confirmation for future strategy tuning.>

Final verdict:
<Validated / Partially validated / Weak edge / Mostly noise / False signal>

Expected by Watchtower:
<yes / probably yes / uncertain / probably no / no>

1000–3000 chars. Dense. Honest. No fluff.
`
