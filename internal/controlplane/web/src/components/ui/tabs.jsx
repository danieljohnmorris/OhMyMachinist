import React, { useId, useState } from "react";
import { cn } from "@/lib/utils";

export function Tabs({ items, label, selected: controlled, onSelect }) {
  const [uncontrolled, setSelected] = useState(items[0].id);
  const id = useId();
  const selected = controlled ?? uncontrolled;
  const active = items.some(item => item.id === selected) ? selected : items[0].id;
  function choose(item) {
    if (onSelect) onSelect(item.id);
    else setSelected(item.id);
  }
  function navigate(event, index) {
    let next;
    if (event.key === "ArrowRight") next = (index + 1) % items.length;
    if (event.key === "ArrowLeft") next = (index + items.length - 1) % items.length;
    if (event.key === "Home") next = 0;
    if (event.key === "End") next = items.length - 1;
    if (next === undefined) return;
    event.preventDefault();
    choose(items[next]);
    document.getElementById(`${id}-tab-${items[next].id}`)?.focus();
  }
  return <div>
    {/* The tab row scrolls sideways on narrow screens. Its own content sits 1px
        below the flex line (the active tab's `-mb-px` underline overlaps the
        rule), and the focus ring paints 4px outside a tab, so the scroll
        container is padded by that much: without the padding the container
        clips both and `overflow-x-auto` turns into a stray vertical scrollbar.
        The negative top margin and `mb-5` keep the row's layout footprint. */}
    <div className="-mt-1 mb-5 overflow-x-auto overflow-y-hidden py-1">
      <div role="tablist" aria-label={label} className="flex gap-4 border-b border-border">
        {items.map((item, index) => <button key={item.id} id={`${id}-tab-${item.id}`} type="button" role="tab" aria-selected={active === item.id} aria-controls={`${id}-panel-${item.id}`} tabIndex={active === item.id ? 0 : -1} onKeyDown={event => navigate(event, index)} onClick={() => choose(item)} className={cn("-mb-px border-b-2 px-0 py-3 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-ring focus-visible:outline-offset-2", active === item.id ? "border-primary text-foreground" : "border-transparent text-muted-foreground hover:text-foreground")}>{item.label}</button>)}
      </div>
    </div>
    {items.map(item => <section key={item.id} id={`${id}-panel-${item.id}`} role="tabpanel" aria-labelledby={`${id}-tab-${item.id}`} hidden={active !== item.id} tabIndex={0} className="focus-visible:outline-2 focus-visible:outline-ring">{item.content}</section>)}
  </div>;
}
