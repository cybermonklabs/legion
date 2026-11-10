import { useCallback, useRef, useState } from "react";

/** An element's width in pixels, measured; before a measure (or where nothing measures, as in a
 *  test) a desktop width, so what a wide page draws is drawn. */
export function useWidth(): [(node: HTMLElement | null) => void, number] {
  const [width, setWidth] = useState(1000);
  const observer = useRef<ResizeObserver | null>(null);
  const ref = useCallback((node: HTMLElement | null) => {
    observer.current?.disconnect();
    if (node === null || typeof ResizeObserver === "undefined") return;
    observer.current = new ResizeObserver(([entry]) => {
      if (entry !== undefined && entry.contentRect.width > 0) setWidth(entry.contentRect.width);
    });
    observer.current.observe(node);
  }, []);
  return [ref, width];
}
