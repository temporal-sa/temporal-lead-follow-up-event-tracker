# QR display assets

`internal/web/static/temporal-stars.avif` is the unmodified starfield texture served by Temporal's website:
https://temporal.io/images/backgrounds/stars.avif
Reference CSS: https://temporal.io/_app/immutable/assets/grid.BKppebuq.css

`internal/web/static/ziggy-peek.png` is a transparent cutout prepared with the built-in image generation tool from the official full-color Ziggy illustration in https://temporal.io/blog/temporal-in-space.
Source image: https://images.ctfassets.net/0uuz8ydxyd9p/1AlHAN5CNjpTdm2liSeerT/4cd2a0760389b7cff6712181c53afa8e/Temporal_Ziggy.png
`internal/web/static/ziggy-blink.png` is the matching closed-eye frame, also prepared with built-in image editing. CSS synchronizes a short blink and a double blink with the peeking cycle.
The animation is CSS; the assets are PNGs, not animated GIFs. Motion is disabled for visitors who prefer reduced motion. Adding `?motion=on` to a QR display URL explicitly enables motion on that page without changing the device preference.

Final image editing prompt:

```text
Use case: background-extraction
Asset type: transparent mascot cutout for a web page, animated later with CSS.
Input images: Image 1 is the edit target: the official Temporal Ziggy illustration.
Primary request: Remove only the solid dark background and its empty margins. Produce a clean cutout on a genuinely transparent alpha background.
Subject: The same lavender-blue smiling eight-legged tardigrade, Ziggy.
Composition/framing: Keep the entire mascot, including every claw and all limbs, in the same pose and orientation as the original. Center it with a small transparent margin; use a square canvas that the mascot substantially fills.
Constraints: Preserve the original illustration faithfully: silhouette, face, eye highlights, one tooth, tongue, pink cheeks, dark outlines, lavender and blue flat colors and shading, and all eight legs with claws. Do not redraw or redesign it. Change only the background and framing.
Avoid: No added objects, stars, scenery, text, watermark, white outline, drop shadow, or opaque background. Do not draw a checkerboard: the background must be actual transparent pixels.
```

Final closed-eye image editing prompt:

```text
Use case: precise-object-edit
Asset type: closed-eye animation frame for the existing transparent Temporal Ziggy web mascot.
Input images: Image 1 is the edit target, the open-eye transparent mascot frame.
Primary request: Close BOTH of Ziggy's eyes for a happy quick blink. Replace the two open black oval eyes and white highlights with two simple curved dark eyelid lines, like happy closed eyes. Remove the open-eye shapes fully in those eye areas. Keep the mouth smiling and everything else exactly the same.
Composition/framing: Retain the exact square canvas dimensions, mascot position, size, angle, and every silhouette pixel. The output will alternate with Image 1 as a blink animation, so the body, mouth, cheeks, outlines, colors, shadows, legs, and transparent margins must stay aligned and identical.
Constraints: Change ONLY the two eye areas. Preserve all eight legs and claws, lavender-blue body, pink cheeks, single tooth, open mouth and tongue. Do not resize, reframe, or redesign the character. No movement of the head or mouth. Maintain real alpha transparency everywhere the original is transparent.
Avoid: No background, stars, text, watermark, accessories, extra facial features, changes to shading or body pose, or drawn checkerboard.
```
