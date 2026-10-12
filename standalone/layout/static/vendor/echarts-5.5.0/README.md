# Apache ECharts 5.5.0 (vendored)

`echarts.min.js` is the unmodified `dist/echarts.min.js` from the npm package
`echarts@5.5.0` (https://registry.npmjs.org/echarts/-/echarts-5.5.0.tgz),
SHA-256 `42f8329d989b6f6539dd2b15bbdf0d82025762ac112fbb60dc57b27d7bcf3946`.
It draws the report page's trend and sums charts on the standalone server
(alitheia serves its own copy of the same file).

Licence: Apache-2.0 (`LICENSE`, `NOTICE`); it embeds parts of d3.js under
BSD-3-Clause (`LICENSE-d3`). These files are third-party and are not covered
by the tracker's AGPL-3.0 licence.

To upgrade, replace the directory with the new version's files, rename it,
update `layout.ChartScript` and the checksum test.
