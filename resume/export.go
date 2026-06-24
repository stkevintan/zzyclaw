package resume

import (
	"fmt"
	"strconv"

	"github.com/xuri/excelize/v2"
)

var headers = []string{
	"序号",
	"联系结果",
	"学科",
	"姓名",
	"性别",
	"身份证号",
	"年龄",
	"民族",
	"籍贯",
	"政治面貌",
	"最高学历",
	"手机号码",
	"职称",
	"毕业院校（第一学历）",
	"专业",
	"毕业院校（最高学历）",
	"专业",
	"毕业时间",
	"教师资格证书类型",
	"工作\n年月",
	"现工作单位",
	"工作经验",
	"主要荣誉",
	"预计税前月薪\n（万/月）",
	"预计税前薪资\n（万/年）",
	"备注",
}

// ExportXLSX creates an xlsx file from a list of results and returns the raw bytes.
// Results are grouped by subject (学科), each in its own sheet.
// Entries with errors or no subject go into a fallback sheet.
func ExportXLSX(results []Result, fallbackSheet string) (_ []byte, err error) {
	f := excelize.NewFile()
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close xlsx: %w", closeErr)
		}
	}()

	if fallbackSheet == "" {
		fallbackSheet = "其他"
	}

	// --- styles ---
	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 12, Family: "宋体"},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create header style: %w", err)
	}

	highlightHeaderStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 12, Family: "宋体"},
		Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"FFFF00"}},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create highlight header style: %w", err)
	}

	textStyle, err := f.NewStyle(&excelize.Style{
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create text style: %w", err)
	}

	cnyStyle, err := f.NewStyle(&excelize.Style{
		CustomNumFmt: strPtr("￥#,##0.0000"),
		Alignment:    &excelize.Alignment{Vertical: "center"},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create currency style: %w", err)
	}

	// Group results by subject, preserving insertion order
	type group struct {
		name    string
		results []Result
	}
	var order []string
	groups := make(map[string]*group)

	for _, r := range results {
		if r.Error != "" {
			continue
		}
		subject := string(r.Entry.Subject)
		if subject == "" {
			subject = fallbackSheet
		}
		g, ok := groups[subject]
		if !ok {
			g = &group{name: subject}
			groups[subject] = g
			order = append(order, subject)
		}
		g.results = append(g.results, r)
	}

	// If no groups, create a single empty sheet
	if len(order) == 0 {
		order = append(order, fallbackSheet)
		groups[fallbackSheet] = &group{name: fallbackSheet}
	}

	// Rename default "Sheet1" to the first group
	defaultSheet := f.GetSheetName(0)
	if err := f.SetSheetName(defaultSheet, order[0]); err != nil {
		return nil, fmt.Errorf("rename default sheet: %w", err)
	}

	// Create remaining sheets
	for _, name := range order[1:] {
		if _, err := f.NewSheet(name); err != nil {
			return nil, fmt.Errorf("create sheet %q: %w", name, err)
		}
	}

	// Populate each sheet
	for _, name := range order {
		g := groups[name]
		if err := writeSheet(f, name, g.results, headerStyle, highlightHeaderStyle, textStyle, cnyStyle); err != nil {
			return nil, err
		}
	}

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, fmt.Errorf("write xlsx: %w", err)
	}
	return buf.Bytes(), nil
}

func writeSheet(f *excelize.File, sheet string, results []Result, headerStyle, highlightHeaderStyle, textStyle, cnyStyle int) error {
	// --- headers ---
	for i, h := range headers {
		cell, err := excelize.CoordinatesToCellName(i+1, 1)
		if err != nil {
			return fmt.Errorf("get header cell name: %w", err)
		}
		if err := f.SetCellValue(sheet, cell, h); err != nil {
			return fmt.Errorf("set header value %s: %w", cell, err)
		}
		style := headerStyle
		if i == 1 { // 联系结果 column gets yellow highlight
			style = highlightHeaderStyle
		}
		if err := f.SetCellStyle(sheet, cell, cell, style); err != nil {
			return fmt.Errorf("set header style %s: %w", cell, err)
		}
	}
	if err := f.SetRowHeight(sheet, 1, 57); err != nil {
		return fmt.Errorf("set header row height: %w", err)
	}

	// --- column widths ---
	colWidths := map[int]float64{
		14: 12.25, 16: 13.25,
	}
	for col, w := range colWidths {
		colName, err := excelize.ColumnNumberToName(col)
		if err != nil {
			return fmt.Errorf("get column name %d: %w", col, err)
		}
		if err := f.SetColWidth(sheet, colName, colName, w); err != nil {
			return fmt.Errorf("set column width %s: %w", colName, err)
		}
	}

	// --- data rows ---
	for i, r := range results {
		row := i + 2
		e := r.Entry

		values := []any{
			i + 1,                                // 序号
			"",                                   // 联系结果
			string(e.Subject),                    // 学科
			string(e.Name),                       // 姓名
			string(e.Gender),                     // 性别
			e.IDNumber,                           // 身份证号
			e.Age,                                // 年龄
			e.Ethnicity,                          // 民族
			e.NativePlace,                        // 籍贯
			string(e.PoliticalStatus),            // 政治面貌
			string(e.HighestEducation),           // 最高学历
			e.PhoneNumber,                        // 手机号码
			string(e.ProfessionalTitle),          // 职称
			e.UndergraduateSchool,                // 毕业院校（第一学历）
			e.UndergraduateMajor,                 // 专业
			e.GraduateSchool,                     // 毕业院校（最高学历）
			e.GraduateMajor,                      // 专业
			e.GraduationDate,                     // 毕业时间
			string(e.TeachingCertificate),        // 教师资格证书类型
			e.WorkStartDate,                      // 工作年月
			e.CurrentEmployer,                    // 现工作单位
			e.WorkExperience,                     // 工作经验
			e.MainHonors,                         // 主要荣誉
			parseSalary(e.ExpectedMonthlySalary), // 预计税前月薪
			parseSalary(e.ExpectedAnnualSalary),  // 预计税前薪资
			e.Remarks,                            // 备注
		}

		for j, v := range values {
			cell, err := excelize.CoordinatesToCellName(j+1, row)
			if err != nil {
				return fmt.Errorf("get data cell name row %d col %d: %w", row, j+1, err)
			}
			if err := f.SetCellValue(sheet, cell, v); err != nil {
				return fmt.Errorf("set data value %s: %w", cell, err)
			}

			// Apply CNY style for salary columns (24, 25)
			if j == 23 || j == 24 {
				if err := f.SetCellStyle(sheet, cell, cell, cnyStyle); err != nil {
					return fmt.Errorf("set currency style %s: %w", cell, err)
				}
			} else {
				if err := f.SetCellStyle(sheet, cell, cell, textStyle); err != nil {
					return fmt.Errorf("set text style %s: %w", cell, err)
				}
			}
		}
	}
	return nil
}

func parseSalary(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func strPtr(s string) *string {
	return &s
}
