#!/usr/bin/env python3
"""Census of credentialed commands and write-shaped MCP calls in Claude Code
transcripts (docs/design/execution-plan.md, P0). Usage: scripts/census.py [glob]"""
import collections, glob, json, os, re, sys
cmds=[]; mcp=collections.Counter(); files=0
for f in glob.glob(sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser('~/.claude/projects/*/*.jsonl')):
    files+=1
    for line in open(f, errors='ignore'):
        if '"tool_use"' not in line: continue
        try: d=json.loads(line)
        except: continue
        for c in (d.get('message') or {}).get('content') or []:
            if not isinstance(c,dict) or c.get('type')!='tool_use': continue
            n=c.get('name','')
            if n=='Bash': cmds.append(c.get('input',{}).get('command',''))
            elif n.startswith('mcp__'): mcp[n]+=1
W={'gh merge':r'\bgh pr merge\b','gh review/approve':r'\bgh pr review\b','gh comment':r'\bgh (pr|issue) comment\b','gh api write':r'\bgh api\b[^|;&]*(-X|--method)\s*(POST|PUT|PATCH|DELETE)|\bgh api graphql\b[^|]*mutation','gh pr create/edit':r'\bgh pr (create|edit|ready)\b','gh workflow/run':r'\bgh (workflow run|run rerun)\b','git push':r'\bgit push\b','pulumi up/destroy':r'\bpulumi (up|destroy|refresh)\b','tofu/terraform apply':r'\b(tofu|terraform) (apply|destroy)\b','aws write':r'\baws \S+ (put|create|delete|update|modify|attach|detach|run|start|stop|terminate|tag|set)-','kubectl write':r'\bkubectl (apply|delete|patch|scale|rollout|edit|annotate|label|create)\b','helm write':r'\bhelm (upgrade|install|uninstall|rollback)\b'}
cnt=collections.Counter(); complex_=collections.Counter()
for c in cmds:
    for k,r in W.items():
        if re.search(r,c):
            cnt[k]+=1
            if re.search(r'\$\(|`|\beval\b|\bbash -c\b|\bsh -c\b|\$\{?[A-Za-z_]',c): complex_[k]+=1
print('transcripts',files,'bash calls',len(cmds),'mcp calls',sum(mcp.values()))
for k,v in cnt.most_common(): print(f'{k:22} {v:6}  with substitution/vars: {complex_[k]} ({100*complex_[k]//v}%)')
w=collections.Counter()
for n,v in mcp.items():
    if re.search(r'(send|save|create|update|delete|post|merge|mutate|provision|apply|reply|forward|trash|upload|set_|manage_|batch)',n.split('__')[-1]): w[n]+=v
print('write-shaped MCP calls',sum(w.values()))
for n,v in w.most_common(12): print(f'  {n} {v}')
