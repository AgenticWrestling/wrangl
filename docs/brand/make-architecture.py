#!/usr/bin/env python3
"""Generates docs/brand/architecture.svg, the README architecture diagram, in the logo's style:
dark ground, glowing ringed nodes, thin parallel circuit traces converging on a central hub.
Run: python3 docs/brand/make-architecture.py   (no dependencies)"""
from pathlib import Path

W, H = 1200, 780
BG, INK, DIM = "#0b0e13", "#e8f1ff", "#8fb4d6"
C = dict(cyan="#2ee6ff", orange="#ff8a4c", purple="#a07cff", green="#4df59a", blue="#5aa8ff")
FONT = "Inter, 'Segoe UI', Helvetica, Arial, sans-serif"
HUB = (600, 400)

# name: (x, y, r, colour, title, subtitle, planned)
NODES = {
    "phone":   (600, 105, 38, "orange", "Phone", "PWA · web push", False),
    "cli":     (295, 130, 28, "orange", "CLI / TUI", "unix socket or HTTPS", False),
    "agents":  (905, 130, 28, "orange", "Your agents", "MCP", False),
    "cf":      (600, 245, 22, "purple", "Cloudflare Access", "", False),
    "gh":      (140, 330, 40, "purple", "GitHub", "author + reviewer Apps", False),
    "aws":     (140, 520, 40, "blue", "AWS", "EC2 · Cost Explorer", False),
    "linux":   (1045, 250, 40, "green", "Linux runner", "arm64 / x86_64 AMI", False),
    "android": (1095, 420, 36, "green", "Android runner", "emulator AMI", True),
    "mac":     (1030, 585, 36, "green", "macOS runner", "EC2 Mac AMI", True),
    "host":    (790, 600, 26, "green", "Host runner", "permanent", False),
    "ask":     (410, 600, 26, "cyan", "Controller agents", "Ask? on a task", False),
}
SIDE = {"phone", "cf"}
# (a, b, colour, label, label position, planned)
LINKS = [
    ("phone", "cf", "orange", "HTTPS", (614, 190), False),
    ("cf", "hub", "purple", "", None, False),
    ("cli", "hub", "orange", "", None, False),
    ("agents", "hub", "orange", "", None, False),
    ("gh", "hub", "purple", "PRs · reviews · webhooks", (300, 322), False),
    ("aws", "hub", "blue", "launch · terminate", (300, 495), False),
    ("linux", "hub", "green", "NATS · TLS", (850, 285), False),
    ("android", "hub", "green", "", None, True),
    ("mac", "hub", "green", "", None, True),
    ("host", "hub", "green", "", None, False),
    ("ask", "hub", "cyan", "", None, False),
]

def pos(n):
    return HUB if n == "hub" else NODES[n][:2]

def radius(n):
    return 74 if n == "hub" else NODES[n][2]

def trace(a, b, col, planned):
    (x1, y1), (x2, y2) = pos(a), pos(b)
    import math
    d = math.hypot(x2 - x1, y2 - y1)
    ux, uy = (x2 - x1) / d, (y2 - y1) / d
    sx, sy = x1 + ux * (radius(a) + 4), y1 + uy * (radius(a) + 4)
    ex, ey = x2 - ux * (radius(b) + 4), y2 - uy * (radius(b) + 4)
    px, py = -uy, ux                      # perpendicular, for the parallel strands
    horiz = abs(ux) > abs(uy)
    out = []
    for k, (off, op, w) in enumerate([(0, 0.95, 1.6), (-5, 0.45, 1), (5, 0.45, 1), (-10, 0.22, .8), (10, 0.22, .8)]):
        ax, ay, bx, by = sx + px * off, sy + py * off, ex + px * off, ey + py * off
        # S-curve: leave and arrive along the dominant axis
        if horiz:
            mx = (ax + bx) / 2
            path = f"M{ax:.1f} {ay:.1f} C{mx:.1f} {ay:.1f} {mx:.1f} {by:.1f} {bx:.1f} {by:.1f}"
        else:
            if abs(bx - ax) < 0.5:
                bx += 0.6  # a zero-width path would get no bbox gradient and vanish
            my = (ay + by) / 2
            path = f"M{ax:.1f} {ay:.1f} C{ax:.1f} {my:.1f} {bx:.1f} {my:.1f} {bx:.1f} {by:.1f}"
        dash = ' stroke-dasharray="3 7"' if planned else ""
        out.append(f'<path d="{path}" fill="none" stroke="url(#g-{col})" stroke-width="{w}" opacity="{op}"{dash} stroke-linecap="round"/>')
    # a bright pulse near the node end of the main strand, like the logo's small satellites
    t = 0.72
    px_, py_ = sx + (ex - sx) * t, sy + (ey - sy) * t
    out.append(f'<circle cx="{px_:.1f}" cy="{py_:.1f}" r="3.2" fill="{C[col]}" filter="url(#glow)"/>')
    return "\n".join(out)

def node(name):
    x, y, r, col, title, sub, planned = NODES[name]
    c = C[col]
    dash = ' stroke-dasharray="5 6"' if planned else ""
    ring = f'<circle cx="{x}" cy="{y}" r="{r}" fill="#0b0e13" stroke="{c}" stroke-width="2.4"{dash} filter="url(#glow)"/>'
    core = f'<circle cx="{x}" cy="{y}" r="{r*0.42:.1f}" fill="{c}" opacity="{0.45 if planned else 0.95}"/>'
    # outer faint halo ring, as in the logo
    halo = f'<circle cx="{x}" cy="{y}" r="{r+9}" fill="none" stroke="{c}" stroke-width="0.8" opacity="0.28"/>'
    if name in SIDE:  # label beside the node, so the vertical traces stay visible
        lx, ty, anchor = x + r + 22, y - 2, "start"
    else:
        lx, ty, anchor = x, y + r + 24, "middle"
    lab = f'<text x="{lx}" y="{ty}" text-anchor="{anchor}" font-family="{FONT}" font-size="15" font-weight="600" fill="{INK}">{title}</text>'
    if sub:
        lab += f'<text x="{lx}" y="{ty+17}" text-anchor="{anchor}" font-family="{FONT}" font-size="10.5" letter-spacing="1.4" fill="{DIM}">{sub.upper()}</text>'
    if planned:
        lab += f'<text x="{lx}" y="{ty+33}" text-anchor="{anchor}" font-family="{FONT}" font-size="10" letter-spacing="1.6" fill="{c}" opacity="0.8">PLANNED</text>'
    return halo + ring + core + lab

def hub():
    x, y = HUB
    return f'''
<circle cx="{x}" cy="{y}" r="150" fill="url(#hubglow)"/>
<circle cx="{x}" cy="{y}" r="86" fill="none" stroke="{C['cyan']}" stroke-width="0.8" opacity="0.3"/>
<circle cx="{x}" cy="{y}" r="74" fill="#0b0e13" stroke="{C['cyan']}" stroke-width="3.2" filter="url(#glow)"/>
<circle cx="{x}" cy="{y}" r="50" fill="none" stroke="#7df3ff" stroke-width="1" opacity="0.5"/>
<circle cx="{x}" cy="{y}" r="34" fill="url(#orb)" filter="url(#glow)"/>
<text x="{x}" y="{y+112}" text-anchor="middle" font-family="{FONT}" font-size="26" font-weight="700" fill="{INK}" stroke="{BG}" stroke-width="7" paint-order="stroke" stroke-linejoin="round">wrangl controller</text>
<text x="{x}" y="{y+132}" text-anchor="middle" font-family="{FONT}" font-size="11" letter-spacing="2" fill="{C['cyan']}" stroke="{BG}" stroke-width="6" paint-order="stroke" stroke-linejoin="round">WEB APP · HTTP API · SQLITE · NATS + JETSTREAM</text>
'''

def build():
    defs = [f'<radialGradient id="orb" cx="40%" cy="38%" r="70%"><stop offset="0" stop-color="#aef8ff"/><stop offset="0.5" stop-color="{C["cyan"]}"/><stop offset="1" stop-color="#1a7fff"/></radialGradient>',
            f'<radialGradient id="hubglow"><stop offset="0" stop-color="{C["cyan"]}" stop-opacity="0.22"/><stop offset="1" stop-color="{C["cyan"]}" stop-opacity="0"/></radialGradient>',
            f'<radialGradient id="bg" cx="50%" cy="50%" r="70%"><stop offset="0" stop-color="#111a26"/><stop offset="1" stop-color="{BG}"/></radialGradient>',
            '<filter id="glow" x="-60%" y="-60%" width="220%" height="220%"><feGaussianBlur stdDeviation="3.2" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>',
            '<filter id="softglow" x="-20%" y="-60%" width="140%" height="220%"><feGaussianBlur stdDeviation="5" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>',
            '<pattern id="dots" width="28" height="28" patternUnits="userSpaceOnUse"><circle cx="2" cy="2" r="1" fill="#1b2736"/></pattern>']
    for name, col in C.items():
        defs.append(f'<linearGradient id="g-{name}" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="{C["cyan"]}"/><stop offset="1" stop-color="{col}"/></linearGradient>')
    parts = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" role="img" aria-labelledby="t d">',
             '<title id="t">wrangl architecture</title>',
             '<desc id="d">A phone, a CLI and your own agents reach the wrangl controller (through Cloudflare Access for remote access). The controller launches runners on AWS EC2 (Linux today; Android emulator and macOS planned) that dial back over NATS, and works with GitHub for pull requests and reviews.</desc>',
             f'<defs>{"".join(defs)}</defs>',
             f'<rect width="{W}" height="{H}" rx="26" fill="url(#bg)"/><rect width="{W}" height="{H}" rx="26" fill="url(#dots)" opacity="0.7"/>',
             f'<rect x="1" y="1" width="{W-2}" height="{H-2}" rx="25" fill="none" stroke="#1d2b3d"/>']
    for a, b, col, label, lp, planned in LINKS:
        parts.append(trace(a, b, col, planned))
        if label:
            parts.append(f'<text x="{lp[0]}" y="{lp[1]}" font-family="{FONT}" font-size="10.5" letter-spacing="1.4" fill="{C[col]}" opacity="0.9">{label.upper()}</text>')
    parts.append(hub())
    for n in NODES:
        parts.append(node(n))
    parts.append(f'<text x="40" y="52" font-family="{FONT}" font-size="22" font-weight="700" fill="{INK}">wrangl</text>'
                 f'<text x="40" y="72" font-family="{FONT}" font-size="10.5" letter-spacing="2.4" fill="{DIM}">METAHARNESS ORCHESTRATOR</text>')
    parts.append(f'<text x="{W/2}" y="{H-26}" text-anchor="middle" font-family="{FONT}" font-size="12" letter-spacing="5" fill="{DIM}">WORK  ·  RUN  ·  ASK  ·  NUDGE  ·  GATE  ·  LAND</text>')
    parts.append('</svg>')
    return "\n".join(parts)

out = Path(__file__).resolve().parent / "architecture.svg"
out.write_text(build())
print("wrote", out)
