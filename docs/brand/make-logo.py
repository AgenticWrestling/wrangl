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

def tight(img, pad, square):
    """Crop to the visible pixels (alpha > 40) plus an even pad, centred; square pads the short side."""
    al = np.asarray(img)[..., 3]
    ys, xs = np.where(al > 40)
    x0, x1, y0, y1 = xs.min(), xs.max() + 1, ys.min(), ys.max() + 1
    w, h = x1 - x0, y1 - y0
    p = int(max(w, h) * pad)
    if square:
        side = max(w, h) + 2 * p
        cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
        box = (round(cx - side / 2), round(cy - side / 2), round(cx + side / 2), round(cy + side / 2))
    else:
        box = (x0 - p, y0 - p, x1 + p, y1 + p)
    canvas = Image.new("RGBA", (box[2] - box[0], box[3] - box[1]), (0, 0, 0, 0))
    canvas.alpha_composite(img, (-box[0], -box[1]))
    return canvas

# the burst: bbox is about x 578-1526, y 415-1299 in the 2000px source. Key a generous crop, clear
# the wordmark glow under the burst (it starts at source y~1240; the burst ends ~1245), then
# crop to the star's own bounds so it sits centred with the same small pad on every side.
cx, cy, half = 1052, 857, 640
mark = crop((cx - half, cy - half, cx + half, cy + half))
a = np.asarray(mark).copy()
a[1252 - (cy - half):, :, 3] = 0
mark = tight(Image.fromarray(a), 0.025, True)
lock = tight(crop((380, 340, 1620, 1680)), 0.03, False)

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
    tile(n, 0.94).save(out / "icons" / f"{n}.png", optimize=True)
tile(512, 0.58).save(out / "icons" / "512-maskable.png", optimize=True)  # inside the 80% safe circle
print("ok")
