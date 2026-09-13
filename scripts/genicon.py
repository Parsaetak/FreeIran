#!/usr/bin/env python3
"""FreeIran icon generator (v0.9.1).

Renders the canonical FreeIran application icon — a brand-green
lightning bolt on a dark rounded square — into every derivative the
project needs:

    assets/freeiran-icon.svg            canonical vector source
    assets/freeiran-icon.ico            Windows executable/window icon
    assets/freeiran-icon.png            256px raster (docs/release)
    internal/appicon/freeiran-icon.png  embedded copy for the runtime
                                        Linux window icon

The geometry (lightning polygon, corner radius, colors) mirrors the
in-app brand mark (frontend/src/components/Icons.tsx), so every format
stays consistent.

Usage:  python3 scripts/genicon.py
"""

from pathlib import Path

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parent.parent
ASSETS = ROOT / "assets"
APPICON = ROOT / "internal" / "appicon"

# Brand palette (mirrors frontend/src/styles/index.css tokens).
BG = (11, 14, 19, 255)           # --bg
BG_EDGE = (16, 20, 27, 255)      # --surface
BORDER = (61, 220, 132, 110)     # --accent-border
GREEN = (61, 220, 132, 255)      # --accent
GREEN_HI = (126, 238, 180, 255)  # --accent-text

# Lightning bolt geometry from the in-app brand mark (24x24 space).
BOLT = [
    (13.0, 1.6),
    (4.6, 13.4),
    (10.9, 13.4),
    (9.6, 22.4),
    (19.4, 10.0),
    (12.3, 10.0),
]

ICO_SIZES = [16, 24, 32, 48, 64, 128, 256]
PNG_SIZE = 256


def scale(points, size):
    """Map the 24x24 geometry onto a size x size canvas."""
    k = size / 24.0
    return [(x * k, y * k) for x, y in points]


def render(size: int) -> Image.Image:
    """Render the icon at the given square size (4x supersampled)."""
    ss = size * 4

    img = Image.new("RGBA", (ss, ss), (0, 0, 0, 0))
    draw = ImageDraw.Draw(img)

    radius = ss * 0.22
    draw.rounded_rectangle(
        [0, 0, ss - 1, ss - 1],
        radius=radius,
        fill=BG,
        outline=BORDER,
        width=max(1, int(ss * 0.02)),
    )

    inset = ss * 0.045
    draw.rounded_rectangle(
        [inset, inset, ss - 1 - inset, ss - 1 - inset],
        radius=radius * 0.86,
        fill=BG_EDGE,
    )

    draw.polygon(scale(BOLT, ss), fill=GREEN)

    # Highlight on the upper facet of the bolt for a little depth.
    hi = [(13.0, 1.6), (19.4, 10.0), (12.3, 10.0)]
    draw.polygon(scale(hi, ss), fill=GREEN_HI)

    return img.resize((size, size), Image.LANCZOS)


def svg_source() -> str:
    """Canonical SVG (256 viewBox) with the same geometry."""
    k = 256 / 24.0
    pts = " ".join(f"{x * k:.2f},{y * k:.2f}" for x, y in BOLT)
    return f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256" width="256" height="256">
  <!-- FreeIran application icon (canonical asset).
       Geometry mirrors the in-app brand mark (frontend/src/components/Icons.tsx).
       Regenerate derivatives with: python3 scripts/genicon.py -->
  <defs>
    <linearGradient id="bolt" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#7eeeb4"/>
      <stop offset="1" stop-color="#3ddc84"/>
    </linearGradient>
  </defs>
  <rect x="4" y="4" width="248" height="248" rx="56" fill="#0b0e13"/>
  <rect x="16" y="16" width="224" height="224" rx="48" fill="#10141b" stroke="#3ddc84" stroke-opacity="0.43" stroke-width="5"/>
  <polygon points="{pts}" fill="url(#bolt)"/>
</svg>
"""


def main() -> None:
    ASSETS.mkdir(exist_ok=True)
    APPICON.mkdir(parents=True, exist_ok=True)

    base = render(256)

    # ICO with the full size ladder (Windows picks per context).
    base.save(ASSETS / "freeiran-icon.ico", sizes=[(s, s) for s in ICO_SIZES])

    # 256px raster for docs / release branding.
    base.save(ASSETS / "freeiran-icon.png")

    # Embedded copy consumed by internal/appicon (//go:embed).
    base.save(APPICON / "freeiran-icon.png")

    # Canonical vector source.
    (ASSETS / "freeiran-icon.svg").write_text(svg_source(), encoding="utf-8")

    print(f"icon: wrote {ASSETS / 'freeiran-icon.ico'} ({len(ICO_SIZES)} sizes)")
    print(f"icon: wrote {ASSETS / 'freeiran-icon.png'} ({PNG_SIZE}px)")
    print(f"icon: wrote {APPICON / 'freeiran-icon.png'} ({PNG_SIZE}px)")
    print(f"icon: wrote {ASSETS / 'freeiran-icon.svg'}")


if __name__ == "__main__":
    main()
