"use strict";

const table = document.querySelector("table");
const body = table?.tBodies[0];
const headers = table?.tHead?.rows[0]?.cells;

if (body && headers) {
  const units = {
    B: 1,
    Ki: 2 ** 10,
    Mi: 2 ** 20,
    Gi: 2 ** 30,
    Ti: 2 ** 40,
    Pi: 2 ** 50,
    Ei: 2 ** 60,
  };
  const storageValue = (text) => {
    const match = text.trim().match(/^([+-]?\d+(?:\.\d+)?)\s+(B|Ki|Mi|Gi|Ti|Pi|Ei)$/);
    return match ? Number(match[1]) * units[match[2]] : null;
  };

  Array.from(headers).forEach((header, column) => {
    const label = header.dataset.sortLabel || header.textContent.trim();
    const button = document.createElement("button");
    const indicator = document.createElement("span");
    button.type = "button";
    button.className = "sort-button";
    button.dataset.sortLabel = label;
    button.setAttribute("aria-label", `Sort ascending by ${label}`);
    button.append(...Array.from(header.childNodes));
    indicator.className = "sort-indicator";
    indicator.setAttribute("aria-hidden", "true");
    indicator.textContent = "↕";
    button.append(indicator);
    header.replaceChildren(button);
    header.setAttribute("aria-sort", "none");

    button.addEventListener("click", () => {
      const direction = header.dataset.sortDirection === "ascending" ? "descending" : "ascending";
      const multiplier = direction === "ascending" ? 1 : -1;

      Array.from(headers).forEach((otherHeader) => {
        otherHeader.dataset.sortDirection = "";
        otherHeader.setAttribute("aria-sort", "none");
        const otherButton = otherHeader.querySelector("button");
        otherButton?.setAttribute("aria-label", `Sort ascending by ${otherButton.dataset.sortLabel}`);
        const otherIndicator = otherButton?.querySelector(".sort-indicator");
        if (otherIndicator) otherIndicator.textContent = "↕";
      });

      header.dataset.sortDirection = direction;
      header.setAttribute("aria-sort", direction);
      button.setAttribute("aria-label", `Sort ${direction === "ascending" ? "descending" : "ascending"} by ${label}`);
      indicator.textContent = direction === "ascending" ? "▲" : "▼";

      const rows = Array.from(body.rows).filter((row) => row.cells.length === headers.length);
      rows.sort((left, right) => {
        const leftText = left.cells[column].textContent.trim();
        const rightText = right.cells[column].textContent.trim();
        let comparison;
        if (column === 0) {
          comparison = leftText.localeCompare(rightText, undefined, { numeric: true, sensitivity: "base" });
        } else {
          const leftValue = storageValue(leftText);
          const rightValue = storageValue(rightText);
          if (leftValue === null || rightValue === null) {
            if (leftValue === rightValue) return 0;
            return leftValue === null ? 1 : -1;
          }
          comparison = leftValue - rightValue;
        }
        return multiplier * comparison;
      });
      rows.forEach((row) => body.appendChild(row));
    });
  });
}
