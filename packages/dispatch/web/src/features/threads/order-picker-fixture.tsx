import type { ReactNode } from "react";

import { useThreadOrder } from "./thread-order";

/** The other thread order, picked through the one preference every thread reads, as the Inbox's
 *  display menu picks it. */
export function OrderPicker(): ReactNode {
  const [order, setOrder] = useThreadOrder();
  return (
    <button onClick={() => setOrder(order === "oldest" ? "newest" : "oldest")} type="button">
      {order === "oldest" ? "Newest first" : "Oldest first"}
    </button>
  );
}
