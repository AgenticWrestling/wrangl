#!/usr/bin/env python3
"""Builds the wrangl logo assets from docs/brand/source.jpg.

The source has a checkerboard baked in as pixels (no real alpha), topping out near 80/255, so
the glow is keyed out by brightness: alpha rises from 0 at 85 to 1 at 190, colour is the hue at
full brightness. Output goes to cmd/wrangl/internal/web/static/.
Run: python3 docs/brand/make-logo.py   (needs Pillow + numpy)
"""
from pathlib import Path
import numpy as np
from PIL import Image

root = Path(__file__).resolve().parent
out = root.parent.parent / "cmd/wrangl/internal/web/static"
BG = (0x0f, 0x11, 0x15)

src = Image.open(root / "source.jpg").convert("RGB")

def key(img):
    a = np.asarray(img).astype(float)
    mx = a.max(2)
    alpha = np.clip((mx - 85) / (190 - 85), 0, 1)
    rgb = a / np.maximum(mx, 1)[..., None] * 255
    return Image.fromarray(np.dstack([rgb, alpha * 255]).astype(np.uint8))

def crop(box):
    return key(src.crop(box))

# the burst: bbox is about x 578-1526, y 415-1299 in the 2000px source
cx, cy, half = 1052, 857, 520
mark = crop((cx - half, cy - half, cx + half, cy + half))
# the wordmark's glow starts just below the burst (source y~1240): clear everything under y=1252 (the burst ends ~1245)
a = np.asarray(mark).copy()
a[1252 - (cy - half):, :, 3] = 0
mark = Image.fromarray(a)
lock = crop((420, 380, 1580, 1640))

mark.resize((256, 256), Image.LANCZOS).save(out / "mark.png", optimize=True)
w = 720
lock.resize((w, int(w * lock.height / lock.width)), Image.LANCZOS).save(out / "logo.png", optimize=True)

def tile(n, scale):
    bg = Image.new("RGBA", (n, n), BG + (255,))
    m = int(n * scale)
    bg.alpha_composite(mark.resize((m, m), Image.LANCZOS), ((n - m) // 2, (n - m) // 2))
    return bg.convert("RGB")

(out / "icons").mkdir(exist_ok=True)
for n in (64, 96, 180, 192, 512):
    tile(n, 0.92).save(out / "icons" / f"{n}.png", optimize=True)
tile(512, 0.62).save(out / "icons" / "512-maskable.png", optimize=True)  # inside the 80% safe circle
print("ok")
