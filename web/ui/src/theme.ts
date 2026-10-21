import { useEffect, useState } from "react";

/**
 * isDarkTheme reads html[data-theme] - main.tsx's one source of truth for
 * the active theme, which @goauthentik/theme itself keys off.
 */
export function isDarkTheme(): boolean {
    return document.documentElement.dataset.theme === "dark";
}

/**
 * useDarkTheme re-renders whenever main.tsx flips html[data-theme], for
 * anything drawn outside CSS (canvas charts, Monaco, inline SVG colors).
 */
export function useDarkTheme(): boolean {
    const [dark, setDark] = useState(isDarkTheme);
    useEffect(() => {
        const observer = new MutationObserver(() => setDark(isDarkTheme()));
        observer.observe(document.documentElement, {
            attributes: true,
            attributeFilter: ["data-theme"],
        });
        return () => observer.disconnect();
    }, []);
    return dark;
}

/**
 * printReport hands the current page to the browser's print dialog ("Save as
 * PDF" there is the report). Light theme while printing: browsers drop
 * backgrounds, which would leave a dark theme's light text on white paper.
 */
export function printReport() {
    const root = document.documentElement;
    const theme = root.dataset.theme;
    root.dataset.theme = "light";
    window.addEventListener("afterprint", () => (root.dataset.theme = theme), { once: true });
    // Give the canvas charts a moment to redraw in the light palette.
    setTimeout(() => window.print(), 300);
}
