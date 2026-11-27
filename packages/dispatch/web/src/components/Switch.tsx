import type { ButtonHTMLAttributes, ReactNode } from "react";

import { switchOffBg, switchOnBg, switchThumbBg } from "../theme/classes";

/** A two-state switch (`role="switch"`): a track coloured by `checked`, and a thumb on the side
 *  it is on. Everything else it says - its name, what a press does, a refusal, a press still out -
 *  is the caller's, handed to the button as it is; `className` adds to the track's classes. */
export function Switch({
  checked,
  className,
  ...button
}: { readonly checked: boolean } & Omit<
  ButtonHTMLAttributes<HTMLButtonElement>,
  "aria-checked" | "children" | "role" | "type"
>): ReactNode {
  return (
    <button
      {...button}
      aria-checked={checked}
      className={`inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors ${className === undefined ? "" : `${className} `}${checked ? switchOnBg : switchOffBg}`}
      role="switch"
      type="button"
    >
      <span
        className={`block h-4 w-4 rounded-full shadow-lg transition-transform ${switchThumbBg} ${checked ? "translate-x-4.5" : "translate-x-0.5"}`}
      />
    </button>
  );
}
