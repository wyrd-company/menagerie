#!/usr/bin/env python3
"""Pack the standalone mockup for offline use or T3's inline HTML renderer."""
import argparse
import base64
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("output", type=Path)
parser.add_argument("--inline", action="store_true", help="Use the host theme and transparent outer canvas")
args = parser.parse_args()
root = Path(__file__).resolve().parent
html = (root / "index.html").read_text()
html = html.replace('<link rel="stylesheet" href="incumbent.css" />', "<style>" + (root / "incumbent.css").read_text() + "</style>")
for name, mime in [("inter.woff2", "font/woff2"), ("jetbrains-mono.woff2", "font/woff2"), ("mark.svg", "image/svg+xml")]:
    uri = "data:" + mime + ";base64," + base64.b64encode((root / name).read_bytes()).decode()
    html = html.replace("url(" + name + ")", "url(" + uri + ")").replace('src="' + name + '"', 'src="' + uri + '"')
if args.inline:
    html = html.replace("</head>", """<style>
:root { --bg:var(--background); --surface:var(--card); --surface-2:var(--secondary);
--text:var(--foreground); --text-muted:var(--muted-foreground); --text-faint:var(--muted-foreground);
--border-strong:var(--border); --brand:var(--foreground); --accent:var(--primary);
--accent-hover:var(--primary); --accent-text:var(--primary-foreground); --accent-wash:var(--secondary);
--ok:var(--success); --ok-wash:var(--secondary); --warn:var(--warning); --warn-wash:var(--warning-surface);
--danger:var(--destructive); --danger-hover:var(--destructive); --danger-wash:var(--destructive-surface);
--neutral:var(--muted-foreground); --neutral-wash:var(--secondary);
--term-bg:var(--code-background); --term-surface:var(--secondary); --term-text:var(--foreground);
--term-muted:var(--muted-foreground); --term-border:var(--border); --term-faint:var(--muted-foreground);
--font-body:var(--font-sans); --font-mono:ui-monospace,monospace; }
html,body {background:transparent;} .shell {padding:0 0 24px;max-width:none;}
</style></head>""")
args.output.write_text(html)
print(args.output)
